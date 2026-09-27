package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dokwalt/dokwalt/internal/api"
)

// Smoke test: the dashboard renders every tab with realistic data and
// fits the terminal. Set DOKWALT_SHOW=1 to print the frames.
func TestDashboardRenders(t *testing.T) {
	m := newDash(t.Context(), &session{name: "prod"})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m.Update(appsMsg{
		{Name: "blog", Stages: []api.Stage{{Name: "production", Status: "running", CurrentRelease: 12, ActiveColor: "green", Domains: []string{"blog.example.com"}}}},
		{Name: "shop", Pipeline: true, Stages: []api.Stage{
			{Name: "production", Status: "running", CurrentRelease: 42, ActiveColor: "blue"},
			{Name: "staging", Status: "degraded", CurrentRelease: 7, ActiveColor: "green"},
		}},
		{Name: "wiki", Stages: []api.Stage{{Name: "production", Status: "not deployed"}}},
	})
	m.Update(topSnapMsg{
		Host:    api.HostMetrics{CPUPct: 23, CPUs: 4, MemUsed: 3 << 30, MemTotal: 8 << 30, DiskUsed: 40 << 30, DiskTotal: 120 << 30, TempC: 54},
		History: map[string][]float64{"blog/production": {1, 3, 2, 8, 4, 2, 1, 5}},
		HTTP:    []api.HTTPMetrics{{Hostname: "blog.example.com", RPS: 12.5, P95: 38, S5xx: 0}},
	})
	m.Update(detailMsg{key: "blog/production",
		svcs: []api.Service{
			{Name: "web", Containers: []api.Container{{State: "running"}, {State: "running"}}, Metrics: &api.LiveMetrics{CPUPct: 4.2, MemBytes: 84 << 20}},
			{Name: "db", Stateful: true, Containers: []api.Container{{State: "running"}}, Metrics: &api.LiveMetrics{CPUPct: 0.8, MemBytes: 41 << 20}},
		},
		rels: []api.Release{
			{Version: 12, Status: "succeeded", Current: true, Description: "New pricing page", CreatedAt: time.Now().Add(-time.Hour)},
			{Version: 11, Status: "failed", Error: "service web is crash-looping", CreatedAt: time.Now().Add(-2 * time.Hour)},
			{Version: 10, Status: "superseded", Description: "Fix typo", CreatedAt: time.Now().Add(-48 * time.Hour)},
		},
		doms: []api.Domain{{Hostname: "blog.example.com", Service: "web", Port: 3000}, {Hostname: "www.blog.example.com", RedirectTo: "blog.example.com"}},
	})
	for _, tab := range []dashTab{tabOverview, tabLogs, tabReleases} {
		m.tab = tab
		if tab == tabLogs {
			m.logs = []string{"12:00:01 web GET / 200 3ms", "12:00:02 db checkpoint complete"}
		}
		out := m.View()
		lines := strings.Split(out, "\n")
		if len(lines) > 32 {
			t.Errorf("tab %d: %d lines, want <= 32", tab, len(lines))
		}
		for i, l := range lines {
			if w := len([]rune(stripANSI(l))); w > 120 {
				t.Errorf("tab %d line %d is %d wide", tab, i, w)
			}
		}
		if os.Getenv("DOKWALT_SHOW") != "" {
			t.Log("\n" + out)
		}
	}
	m.help = true
	if !strings.Contains(m.View(), "Keys") {
		t.Error("help overlay missing")
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			esc = true
		case esc && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'):
			esc = false
		case !esc:
			b.WriteRune(r)
		}
	}
	return b.String()
}
