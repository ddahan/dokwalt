package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const siteDocs = "../../../site/docs"

// The website's docs must match the topics: this fails until `make docs-site` is run.
func TestSiteDocsUpToDate(t *testing.T) {
	index, err := os.ReadFile(filepath.Join(siteDocs, "index.html"))
	if err != nil {
		t.Fatalf("site/docs not generated: run `make docs-site` (%v)", err)
	}
	// The release the pages describe is chosen at generation time; compare with that one.
	m := regexp.MustCompile(`<meta name="dokwalt-version" content="([^"]+)">`).FindSubmatch(index)
	if m == nil {
		t.Fatal("site/docs/index.html has no dokwalt-version meta: run `make docs-site`")
	}
	files, err := generate(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(siteDocs, rel))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("site/docs/%s is out of date with internal/docs/topics: run `make docs-site`", rel)
		}
	}
	entries, _ := os.ReadDir(siteDocs)
	for _, e := range entries {
		if _, ok := files[e.Name()+"/index.html"]; e.IsDir() && !ok {
			t.Errorf("site/docs/%s/ has no topic anymore: run `make docs-site`", e.Name())
		}
	}
}

func TestShellHighlight(t *testing.T) {
	got := hiShell(`sudo dokwalt config:set KEY="a b" -a blog   # note`, true)
	for _, want := range []string{
		`<span class="t-cmd">sudo</span>`,
		`<span class="t-cmd">dokwalt</span>`,
		`KEY=&#34;a b&#34;`,
		`<span class="t-flag">-a</span>`,
		`<span class="t-com"># note</span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hiShell: missing %s in\n%s", want, got)
		}
	}
	if strings.Contains(got, `t-cmd">config:set`) {
		t.Errorf("hiShell: only the command word is a command:\n%s", got)
	}
}

func TestDiagram(t *testing.T) {
	src := "# T\n\n*S.*\n\n```text diagram=architecture\n+--+\n```\n"
	p, err := renderTopic("t", []byte(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := string(p.Body)
	if !strings.Contains(body, `<figure class="diagram">`) || strings.Contains(body, "+--+") {
		t.Errorf("text diagram not replaced by its SVG:\n%.300s", body)
	}
	if _, err := renderTopic("t", []byte(strings.Replace(src, "architecture", "nope", 1)), nil); err == nil {
		t.Error("an unknown diagram must fail generation")
	}
}

func TestTopicPage(t *testing.T) {
	src := "# Deploying\n\n*How `dokwalt deploy` works.*\n\n## Usage\n\nSee `dokwalt docs config` and `dokwalt docs deploy`.\n\n```bash\ndokwalt deploy\n```\n"
	p, err := renderTopic("deploy", []byte(src), map[string]bool{"deploy": true, "config": true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Deploying" || p.SummaryText != "How dokwalt deploy works." {
		t.Errorf("header: %q / %q", p.Title, p.SummaryText)
	}
	body := string(p.Body)
	if strings.Contains(body, "<h1") || strings.Contains(body, "<em>How") {
		t.Errorf("title and summary must not be repeated in the body:\n%s", body)
	}
	if !strings.Contains(body, `<a class="xref" href="/docs/config/"><code>dokwalt docs config</code></a>`) {
		t.Errorf("cross-reference not linked:\n%s", body)
	}
	if strings.Contains(body, `href="/docs/deploy/"`) {
		t.Errorf("a page must not link to itself:\n%s", body)
	}
	if len(p.TOC) != 1 || p.TOC[0].ID != "usage" {
		t.Errorf("toc: %+v", p.TOC)
	}
}
