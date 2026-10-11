// Command sitegen renders the `dokwalt docs` topics into the website's docs pages
// (site/docs): one page per topic, an index, and a search index.
//
// The topics in internal/docs/topics are the single source: the website and the CLI
// always show the same text. Run `make docs-site` after changing a topic; `go test`
// fails while site/docs is out of date. The site documents the latest release only.
//
//	go run ./internal/docs/sitegen [-out site/docs] [-version v0.3.0]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ddahan/dokwalt/internal/docs"
)

// Groups shape the sidebar, the index and the previous/next links. Every topic must
// appear in exactly one group: generation fails otherwise, so a new topic is never
// silently missing from the website.
var groups = []struct {
	Title  string
	Topics []string
}{
	{"Start here", []string{"getting-started", "concepts"}},
	{"Deploy & ship", []string{"deploy", "compose", "config", "domains", "releases", "pipelines"}},
	{"Data", []string{"databases", "backups"}},
	{"Operate", []string{"logs", "monitoring", "alerts", "troubleshooting"}},
	{"Servers", []string{"server", "reboot-recovery", "security"}},
	{"Reference", []string{"commands"}},
}

func main() {
	out := flag.String("out", "site/docs", "output directory")
	version := flag.String("version", "", "release the docs describe (default: the latest git tag)")
	flag.Parse()
	if *version == "" {
		b, err := exec.Command("git", "describe", "--tags", "--abbrev=0").Output()
		if err != nil {
			fatal(fmt.Errorf("no -version and no git tag: %w", err))
		}
		*version = strings.TrimSpace(string(b))
	}
	files, err := generate(*version)
	if err != nil {
		fatal(err)
	}
	if err := write(*out, files); err != nil {
		fatal(err)
	}
	fmt.Printf("Wrote %d files to %s (docs for %s)\n", len(files), *out, *version)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "sitegen:", err)
	os.Exit(1)
}

// generate returns every generated file, keyed by its path relative to the output dir.
func generate(version string) (map[string][]byte, error) {
	known := map[string]bool{}
	for _, t := range docs.Topics() {
		known[t.Name] = true
	}
	var order []string
	seen := map[string]bool{}
	for _, g := range groups {
		for _, name := range g.Topics {
			if !known[name] {
				return nil, fmt.Errorf("group %q lists unknown topic %q", g.Title, name)
			}
			if seen[name] {
				return nil, fmt.Errorf("topic %q is in two groups", name)
			}
			seen[name] = true
			order = append(order, name)
		}
	}
	for name := range known {
		if !seen[name] {
			return nil, fmt.Errorf("topic %q is in no group: add it to groups in sitegen/main.go", name)
		}
	}

	pages := make([]*page, len(order))
	for i, name := range order {
		src, _ := docs.Get(name)
		p, err := renderTopic(name, []byte(src), known)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		pages[i] = p
	}
	bySlug := map[string]*page{}
	for _, p := range pages {
		bySlug[p.Slug] = p
	}
	var nav []navGroup
	for _, g := range groups {
		ng := navGroup{Title: g.Title}
		for _, name := range g.Topics {
			bySlug[name].Group = g.Title
			ng.Pages = append(ng.Pages, bySlug[name])
		}
		nav = append(nav, ng)
	}

	files := map[string][]byte{}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "index", indexData{Version: version, Groups: nav}); err != nil {
		return nil, err
	}
	files["index.html"] = buf.Bytes()
	for i, p := range pages {
		d := topicData{Version: version, Groups: nav, Page: p}
		if i > 0 {
			d.Prev = pages[i-1]
		}
		if i < len(pages)-1 {
			d.Next = pages[i+1]
		}
		var b bytes.Buffer
		if err := tmpl.ExecuteTemplate(&b, "topic", d); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Slug, err)
		}
		files[p.Slug+"/index.html"] = b.Bytes()
	}

	var entries []searchEntry
	for _, p := range pages {
		entries = append(entries, p.search...)
	}
	idx, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	files["search.json"] = append(idx, '\n')
	return files, nil
}

// write replaces the generated files in dir. Hand-written assets (docs.css, docs.js)
// are left alone; topic folders that no longer exist are removed.
func write(dir string, files map[string][]byte) error {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			if _, ok := files[e.Name()+"/index.html"]; !ok {
				if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
					return err
				}
			}
		}
	}
	for rel, b := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
