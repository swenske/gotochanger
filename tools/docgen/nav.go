package main

import "strings"

// NavLink is one clickable entry in the guide's left-hand table of contents.
type NavLink struct {
	Href  string
	Label string
}

// NavGroup is a labeled cluster of NavLinks, rendered as one section of the
// left-hand nav (e.g. "Concepts", "Cookbook").
type NavGroup struct {
	Label string
	Links []NavLink
}

// groupLabelFromDirName turns a source directory name like "08-cookbook"
// into a nav group label like "Cookbook", so any future docs/guide/
// subdirectory becomes a nav group without extra configuration.
func groupLabelFromDirName(name string) string {
	name = strings.TrimLeft(name, "0123456789")
	name = strings.TrimPrefix(name, "-")
	words := strings.Split(name, "-")
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
