// Package docs embeds the user documentation shown by `dokwalt docs`.
package docs

import (
	"embed"
	"sort"
	"strings"
)

//go:embed topics/*.md
var fs embed.FS

type Topic struct {
	Name    string
	Title   string
	Summary string
}

// Order in which topics are listed.
var order = []string{
	"getting-started", "concepts", "deploy", "compose", "config", "domains", "databases", "releases",
	"pipelines", "logs", "monitoring", "alerts", "reboot-recovery", "raspberry-pi", "vps", "security",
	"troubleshooting", "commands",
}

func Get(name string) (string, bool) {
	b, err := fs.ReadFile("topics/" + name + ".md")
	return string(b), err == nil
}

func Topics() []Topic {
	entries, _ := fs.ReadDir("topics")
	rank := map[string]int{}
	for i, n := range order {
		rank[n] = i + 1
	}
	var out []Topic
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".md")
		body, _ := Get(name)
		t := Topic{Name: name, Title: name}
		lines := strings.Split(body, "\n")
		for _, l := range lines {
			l = strings.TrimSpace(l)
			switch {
			case strings.HasPrefix(l, "# ") && t.Title == name:
				t.Title = strings.TrimPrefix(l, "# ")
			case strings.HasPrefix(l, "*") && strings.HasSuffix(l, "*") && t.Summary == "":
				t.Summary = strings.Trim(l, "*")
			}
			if t.Summary != "" {
				break
			}
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank[out[i].Name], rank[out[j].Name]
		if ri == 0 {
			ri = 1000
		}
		if rj == 0 {
			rj = 1000
		}
		return ri < rj
	})
	return out
}

// Search returns topics whose text contains every word of the query.
func Search(q string) []Topic {
	words := strings.Fields(strings.ToLower(q))
	var out []Topic
	for _, t := range Topics() {
		body, _ := Get(t.Name)
		body = strings.ToLower(body)
		ok := true
		for _, w := range words {
			if !strings.Contains(body, w) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, t)
		}
	}
	return out
}
