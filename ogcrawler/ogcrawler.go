package ogcrawler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
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
	Site     string
	Out      io.Writer
	Log      *log.Logger
	Depth    int
	Parallel int
	Get      func(string) (*http.Response, error)
}

type site struct {
	URL    *url.URL
	Parent *url.URL
	Depth  int
}

var pages []*Page
var notFoundErrors []string

// Run - Starts process
func (c *Crawler) Run() (*Website, error) {
	// Clears slice on each new request
	pages = pages[:0]
	notFoundErrors = notFoundErrors[:0]

	if err := c.validate(); err != nil {
		return nil, err
	}
	if c.Get == nil {
		c.Get = http.Get
	}

	queue, sites, wait := makeQueue()

	wait <- 1

	var wg sync.WaitGroup
	for i := 0; i < parallel(c.Parallel); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.worker(sites, queue, wait)
		}()
	}

	s, err := url.Parse(c.Site)
	if err != nil {
		panic(err)
	}

	queue <- site{
		URL:    s,
		Parent: nil,
		Depth:  c.Depth,
	}

	wg.Wait()

	return &Website{Pages: pages, URL: c.Site, Errors: notFoundErrors}, nil
}

func (c Crawler) validate() error {
	if len(c.Site) == 0 {
		return errors.New("No site given")
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

func toURLs(links []string, parse func(string) (*url.URL, error)) ([]*url.URL, error) {
	var invalids []string
	var urls []*url.URL
	var err error

	for _, s := range links {
		u, err := parse(s)
		if err != nil {
			invalids = append(invalids, fmt.Sprintf("%s (%v)", s, err))
			continue
		}

		if u.Scheme == "" {
			u.Scheme = "https"
		}

		if u.Scheme == "http" || u.Scheme == "https" {
			urls = append(urls, u)
		}
	}
	if len(invalids) > 0 {
		err = fmt.Errorf("invalid URLs: %v", strings.Join(invalids, ", "))
	}
	return urls, err
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
	visited := make(map[string]bool)

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
				visited[u] = true
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
) {
	for s := range sites {
		links, err := crawlPage(s, c.Get)
		if err != nil {
			c.Log.Println(err)
		}

		urls, err := toURLs(links, s.URL.Parse)
		if err != nil {
			c.Log.Println(err)
		}
		wait <- len(urls) - 1

		// Submit links to queue in goroutine to not block workers
		go queueURLs(queue, urls, s.URL, s.Depth-1)
	}
}

// crawlPage - Crawls every page while getting og data
func crawlPage(s site, get func(string) (*http.Response, error)) ([]string, error) {
	u := s.URL
	isExternal := s.Parent != nil && s.URL.Host != s.Parent.Host

	r, err := get(u.String())
	if err != nil {
		return nil, fmt.Errorf("failed to get %v: %v", u, err)
	}
	defer r.Body.Close()

	if r.StatusCode >= 404 && r.StatusCode < 800 {
		notFoundErrors = append(notFoundErrors, fmt.Sprintf("%d %v", r.StatusCode, u))
	}

	if r.Request.URL.Host != u.Host {
		isExternal = true
	}

	if isExternal || s.Depth == 1 {
		return nil, err
	}

	p := Page{}.getOgData(u.String())

	pages = append(pages, &p)

	links, err := getLinks(r.Body)

	return links, err
}

// getLinks - Get all <a> tags on page
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
					tl := trimHash(a.Val)
					links = append(links, tl)
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

// Checks if link contains hash, proceeds to trim if true
func trimHash(l string) string {
	if strings.Contains(l, "#") {
		var index int
		for n, str := range l {
			if strconv.QuoteRune(str) == "'#'" {
				index = n
				break
			}
		}
		return l[:index]
	}

	return l
}
