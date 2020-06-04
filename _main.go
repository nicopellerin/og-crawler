package main

import (
	"encoding/json"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"

	"github.com/nicopellerin/og-crawler/ogcrawler"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")

		siteURL, _ := ioutil.ReadAll(r.Body)
		defer r.Body.Close()

		var output io.Writer = os.Stdout
		var errWriter io.Writer = os.Stderr
		errs := log.New(errWriter, "", 0)
		c := ogcrawler.Crawler{Site: string(siteURL), Out: output, Log: errs}
		website, _ := c.Run()

		bs, err := json.Marshal(website)
		if err != nil {
			panic(err)
		}

		w.Write(bs)
	})

	log.Fatal(http.ListenAndServe(":5000", nil))
}
