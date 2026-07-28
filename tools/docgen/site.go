package main

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/yuin/goldmark"
)

//go:embed templates
var templatesFS embed.FS

var pageTemplate = template.Must(template.ParseFS(templatesFS, "templates/guide.html.tmpl"))

type pageData struct {
	IsEmbed     bool
	AssetPrefix string
	BackHref    string
	BackLabel   string
	DocsHref    string // empty omits every "API Docs" link/mention
	NavGroups   []NavGroup
	Content     template.HTML
}

// Build reads every docs/guide/*.md file (and one level of subdirectories,
// each becoming a single nav group - see groupLabelFromDirName) in
// directory order, renders them into one concatenated page, and writes it
// to outDir per target ("embed" or "site").
func Build(md goldmark.Markdown, srcDir, outDir, target, cssPath string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", srcDir, err)
	}

	used := usedHeadingIDs{}
	var groups []NavGroup
	var body strings.Builder

	for _, e := range entries {
		full := filepath.Join(srcDir, e.Name())
		if e.IsDir() {
			group, html, err := renderDir(md, full, used)
			if err != nil {
				return err
			}
			groups = append(groups, group)
			body.WriteString(html)
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		src, err := os.ReadFile(full)
		if err != nil {
			return fmt.Errorf("reading %s: %w", full, err)
		}
		result, err := renderFile(md, src, used)
		if err != nil {
			return fmt.Errorf("rendering %s: %w", full, err)
		}
		groups = append(groups, result.Groups...)
		body.WriteString(result.HTML)
	}

	data := pageData{NavGroups: groups, Content: template.HTML(body.String())}
	switch target {
	case "embed":
		data.IsEmbed = true
		data.AssetPrefix = "/assets/guide/"
		data.BackHref = "/"
		data.BackLabel = "← Back to the app"
		data.DocsHref = "/docs"
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
		return writePage(data, filepath.Join(outDir, "index.html"))
	case "site":
		data.AssetPrefix = "assets/"
		data.BackHref = "https://github.com/swenske/gotochanger"
		data.BackLabel = "← gotochanger on GitHub"
		data.DocsHref = ""
		if err := os.MkdirAll(filepath.Join(outDir, "assets"), 0o755); err != nil {
			return err
		}
		if err := writePage(data, filepath.Join(outDir, "index.html")); err != nil {
			return err
		}
		if err := copyFile(cssPath, filepath.Join(outDir, "assets", "guide.css")); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(outDir, ".nojekyll"), nil, 0o644)
	default:
		return fmt.Errorf("unknown target %q", target)
	}
}

// renderDir renders every *.md file directly inside dir (no further
// recursion) as the flat links of one nav group named after the directory.
func renderDir(md goldmark.Markdown, dir string, used usedHeadingIDs) (NavGroup, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return NavGroup{}, "", fmt.Errorf("reading %s: %w", dir, err)
	}
	group := NavGroup{Label: groupLabelFromDirName(filepath.Base(dir))}
	var body strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		src, err := os.ReadFile(full)
		if err != nil {
			return NavGroup{}, "", fmt.Errorf("reading %s: %w", full, err)
		}
		result, err := renderFile(md, src, used)
		if err != nil {
			return NavGroup{}, "", fmt.Errorf("rendering %s: %w", full, err)
		}
		group.Links = append(group.Links, NavLink{Href: "#" + result.ID, Label: result.Title})
		body.WriteString(result.HTML)
	}
	return group, body.String(), nil
}

func writePage(data pageData, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return pageTemplate.Execute(f, data)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
