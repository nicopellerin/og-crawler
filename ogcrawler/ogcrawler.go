package ogcrawler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const defaultParallel = 15

type Website struct {
	Pages  []*Page  `json:"pages"`
	URL    string   `json:"url"`
	Errors []string `json:"errors"`
}

type Page struct {
	OpenGraph
}

type Image struct {
	URL  string `json:"url"`
	Type string `json:"type"`
}

type OpenGraph struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Image       Image  `json:"image"`
	PageURL     string `json:"pageUrl"`
}

type Crawler struct {
	Sites    []string
	Out      io.Writer
	Log      *log.Logger
	Depth    int
	Parallel int
	Get      func(string) (*http.Response, error)
	Verbose  bool
}

type site struct {
	URL    *url.URL
	Parent *url.URL
	Depth  int
}

var pages []*Page
var notFoundErrors []string

func (c *Crawler) Run(siteUrl string) (*Website, error) {
	// Clears slice on each new request
	pages = pages[:0]
	notFoundErrors = notFoundErrors[:0]

	if err := c.validate(); err != nil {
		return nil, err
	}
	if c.Get == nil {
		c.Get = http.Get
	}
	urls, err := toURLs(c.Sites, url.Parse)
	if err != nil {
		return nil, err
	}

	results := make(chan string)
	defer close(results)
	go func() {
		for r := range results {
			if _, err := fmt.Fprintln(c.Out, r); err != nil {
				c.Log.Printf("failed to write output '%s': %v\n", r, err)
			}
		}
	}()

	queue, sites, wait := makeQueue()

	wait <- len(urls)

	var wg sync.WaitGroup
	for i := 0; i < parallel(c.Parallel); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.worker(sites, queue, wait, results)
		}()
	}

	for _, u := range urls {
		queue <- site{
			URL:    u,
			Parent: nil,
			Depth:  c.Depth,
		}
	}

	wg.Wait()

	return &Website{Pages: pages, URL: siteUrl, Errors: notFoundErrors}, nil
}

func (c Crawler) validate() error {
	if len(c.Sites) == 0 {
		return errors.New("No sites given")
	}
	if c.Out == nil {
		return errors.New("No output writer given")
	}
	if c.Log == nil {
		return errors.New("No error logger given")
	}
	if c.Depth < 0 {
		return errors.New("Depth cannot be negative")
	}
	if c.Parallel < 0 {
		return errors.New("Parallel cannot be negative")
	}
	return nil
}

func toURLs(links []string, parse func(string) (*url.URL, error)) (urls []*url.URL, err error) {
	var invalids []string
	for _, s := range links {
		u, e := parse(s)
		if e != nil {
			invalids = append(invalids, fmt.Sprintf("%s (%v)", s, e))
			continue
		}
		// Default to https
		if u.Scheme == "" {
			u.Scheme = "https"
		}
		// Ignore invalid protocols
		if u.Scheme == "http" || u.Scheme == "https" {
			urls = append(urls, u)
		}
	}
	if len(invalids) > 0 {
		err = fmt.Errorf("invalid URLs: %v", strings.Join(invalids, ", "))
	}
	return
}

func parallel(p int) int {
	if p < 1 {
		return defaultParallel
	}
	return p
}

func makeQueue() (chan<- site, <-chan site, chan<- int) {
	queueCount := 0
	wait := make(chan int)
	sites := make(chan site)
	queue := make(chan site)
	visited := map[string]struct{}{}

	go func() {
		for delta := range wait {
			queueCount += delta
			if queueCount == 0 {
				close(queue)
			}
		}
	}()

	go func() {
		for s := range queue {
			u := s.URL.String()
			if _, v := visited[u]; !v {
				visited[u] = struct{}{}
				sites <- s
			} else {
				wait <- -1
			}
		}
		close(sites)
		close(wait)
	}()

	return queue, sites, wait
}

func (c Crawler) worker(
	sites <-chan site,
	queue chan<- site,
	wait chan<- int,
	results chan<- string,
) {
	for s := range sites {
		if c.Verbose {
			c.Log.Printf("verbose: GET %s\n", s.URL)
		}

		links, shouldUpdate, err := crawlSite(s, c.Get)

		if err != nil {
			parent := ""
			if s.Parent != nil {
				parent = fmt.Sprintf(" on page %v", s.Parent)
			}
			c.Log.Printf("%v%s\n", err, parent)
		}

		if shouldUpdate {
			s.URL.Scheme = "http"
			results <- fmt.Sprintf("%v %v", s.Parent, s.URL.String())
		}

		urls, err := toURLs(links, s.URL.Parse)
		if err != nil {
			c.Log.Printf("page %v: %v\n", s.URL, err)
		}

		wait <- len(urls) - 1

		// Submit links to queue in goroutine to not block workers
		go queueURLs(queue, urls, s.URL, s.Depth-1)

	}
}

func crawlSite(s site, get func(string) (*http.Response, error)) ([]string, bool, error) {
	u := s.URL
	isExternal := s.Parent != nil && s.URL.Host != s.Parent.Host

	// If an external link is http we try https.
	// If it fails it is ignored and we carry on normally.
	// On success we return it as a result.
	if isExternal && u.Scheme == "http" {
		u.Scheme = "https"
		r2, err := get(u.String())
		if err == nil {
			defer r2.Body.Close()
			if r2.StatusCode < 400 {
				return nil, true, nil
			}
		}
		u.Scheme = "http"
	}

	r, err := get(u.String())
	if err != nil {
		return nil, false, fmt.Errorf("failed to get %v: %v", u, err)
	}
	defer r.Body.Close()

	if r.StatusCode >= 404 && r.StatusCode < 800 {
		notFoundErrors = append(notFoundErrors, fmt.Sprintf("%d %v", r.StatusCode, u))
	}

	// Stop when redirecting to external page
	if r.Request.URL.Host != u.Host {
		isExternal = true
	}

	// Stop when site is external.
	// Also stop if depth one is reached, ignored when depth is set to 0.
	if isExternal || s.Depth == 1 {
		return nil, false, err
	}

	p := Page{}.getOgData(u.String())

	pages = append(pages, &p)

	links, err := getLinks(r.Body)

	return links, false, err
}

func getLinks(r io.Reader) ([]string, error) {
	var links []string

	doc, err := html.Parse(r)
	if err != nil {
		return links, fmt.Errorf("failed to parse HTML: %v\n", err)
	}

	var f func(n *html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, a := range n.Attr {
				if a.Key == "href" {
					links = append(links, a.Val)
					break
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)

	return links, nil
}

func queueURLs(queue chan<- site, urls []*url.URL, parent *url.URL, depth int) {
	for _, u := range urls {
		queue <- site{
			URL:    u,
			Parent: parent,
			Depth:  depth,
		}
	}
}

// // Gets Og data related to page
func (p Page) getOgData(uri string) Page {
	res, err := http.Get(uri)
	if err != nil {
		panic(err)
	}

	err = p.ProcessHTML(res.Body)
	if err != nil {
		fmt.Println(err)
	}

	p.OpenGraph.PageURL = uri

	return p
}

func fixUrl(href, base string) string {
	uri, err := url.Parse(href)
	if err != nil {
		return ""
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return ""
	}
	uri = baseURL.ResolveReference(uri)
	return uri.String()
}

func (og *OpenGraph) ToJSON() ([]byte, error) {
	return json.Marshal(og)
}

func (og *OpenGraph) String() string {
	data, err := og.ToJSON()
	if err != nil {
		return err.Error()
	}

	return string(data[:])
}

func (p *Page) ProcessHTML(buffer io.Reader) error {
	z := html.NewTokenizer(buffer)

	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if z.Err() == io.EOF {
				return nil
			}

			return z.Err()
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			name, hasAttr := z.TagName()
			if atom.Lookup(name) == atom.Body {
				return nil
			}
			if atom.Lookup(name) != atom.Meta || !hasAttr {
				continue
			}
			m := make(map[string]string)
			var key, val []byte
			for hasAttr {
				key, val, hasAttr = z.TagAttr()
				m[atom.String(key)] = string(val)
			}
			p.ProcessMeta(m)
		}
	}
}

func (og *OpenGraph) ProcessMeta(metaAttrs map[string]string) {
	switch metaAttrs["property"] {
	case "og:description":
		og.Description = metaAttrs["content"]
	case "og:title":
		og.Title = metaAttrs["content"]
	case "og:url":
		og.URL = metaAttrs["content"]
	case "og:image":
		og.Image.URL = metaAttrs["content"]
	case "og:image:type":
		og.Image.Type = metaAttrs["content"]
	}
}
