// Command loam-docs writes the user docs that loam-api serves at /docs as a
// static site, for the project site (site/, deployed to GitHub Pages by
// .github/workflows/pages.yml):
//
//	go run ./cmd/loam-docs -out _site
//
// The pages land under <out>/docs, at the same routes as on loam-api.
package main

import (
	"flag"
	"log"

	"github.com/arugula-salad/loam/docs"
	"github.com/arugula-salad/loam/internal/docsite"
)

func main() {
	out := flag.String("out", "_site", "the directory to write the site into")
	flag.Parse()
	site, err := docsite.New(docs.FS)
	if err != nil {
		log.Fatal(err)
	}
	if err := site.WriteDir(*out); err != nil {
		log.Fatal(err)
	}
}
