// Command docgen renders the Markdown guide sources under docs/guide/ into
// the two output targets described in docs/guide's own build step: the HTML
// embedded via go:embed for /guide, and a self-contained static export for
// external publishing. See `make guide` / `make site`.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

func main() {
	target := flag.String("target", "", "output target: embed or site")
	src := flag.String("src", "docs/guide", "guide Markdown source directory")
	out := flag.String("out", "", "output directory")
	css := flag.String("css", "internal/api/static/guide/guide.css", "hand-maintained stylesheet to reference/copy")
	flag.Parse()

	if *out == "" || (*target != "embed" && *target != "site") {
		fmt.Fprintln(os.Stderr, "usage: docgen -target=embed|site -src=docs/guide -out=<dir>")
		os.Exit(2)
	}

	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)

	if err := Build(md, *src, *out, *target, *css); err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}
}
