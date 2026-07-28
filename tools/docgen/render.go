package main

import (
	"bytes"
	"fmt"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// fileResult is the outcome of rendering a single docs/guide/*.md file.
type fileResult struct {
	Title string // the file's H1 text, used for the page <title> and as a
	// fallback nav link label when the file has no H2 subsections.
	ID     string // the H1's anchor id.
	HTML   string // rendered body fragment for this file.
	Groups []NavGroup
}

// usedHeadingIDs tracks every heading id assigned so far across the whole
// run. goldmark's WithAutoHeadingID only guarantees uniqueness within a
// single Parse call, but every file here is parsed separately and then
// concatenated into one page, so ids need deduplicating across files too -
// done by rewriting the node's "id" attribute in place before rendering.
type usedHeadingIDs map[string]bool

func (u usedHeadingIDs) claim(id string) string {
	if !u[id] {
		u[id] = true
		return id
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", id, n)
		if !u[candidate] {
			u[candidate] = true
			return candidate
		}
	}
}

// renderFile parses and renders one Markdown file, deriving its nav
// contribution from H1 (title, not itself a link), H2 (nav group), and H3
// (flat link within the current group) headings, in document order.
func renderFile(md goldmark.Markdown, src []byte, used usedHeadingIDs) (fileResult, error) {
	doc := md.Parser().Parse(text.NewReader(src))

	var result fileResult
	var curGroup *NavGroup

	err := ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		h, ok := n.(*ast.Heading)
		if !ok {
			return ast.WalkContinue, nil
		}

		id := headingID(h)
		id = used.claim(id)
		h.SetAttribute([]byte("id"), []byte(id))
		label := string(h.Text(src))

		switch h.Level {
		case 1:
			if result.Title == "" {
				result.Title = label
				result.ID = id
			}
		case 2:
			result.Groups = append(result.Groups, NavGroup{Label: label})
			curGroup = &result.Groups[len(result.Groups)-1]
		case 3:
			if curGroup != nil {
				curGroup.Links = append(curGroup.Links, NavLink{Href: "#" + id, Label: label})
			}
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return fileResult{}, err
	}

	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, doc); err != nil {
		return fileResult{}, err
	}
	result.HTML = buf.String()

	// A file with no H2 at all still needs to be reachable from the nav -
	// fall back to a single group/link pointing at its own H1.
	if len(result.Groups) == 0 && result.Title != "" {
		result.Groups = []NavGroup{{
			Label: result.Title,
			Links: []NavLink{{Href: "#" + result.ID, Label: result.Title}},
		}}
	}

	return result, nil
}

// headingID reads the id WithAutoHeadingID already assigned to h during
// parsing (always present - the parser is configured with that option in
// main.go).
func headingID(h *ast.Heading) string {
	v, ok := h.AttributeString("id")
	if !ok {
		return ""
	}
	b, _ := v.([]byte)
	return string(b)
}
