package cli

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ddahan/dokwalt/internal/api"
)

func values(cands []candidate) []string {
	var out []string
	for _, c := range cands {
		out = append(out, c.value)
	}
	return out
}

func TestSplitLine(t *testing.T) {
	for in, want := range map[string][]string{
		`config:set FOO=bar`:           {"config:set", "FOO=bar"},
		`config:set MSG="hello world"`: {"config:set", "MSG=hello world"},
		`exec -- sh -c 'echo $HOME'`:   {"exec", "--", "sh", "-c", "echo $HOME"},
		`  logs   -n 10 `:              {"logs", "-n", "10"},
		`config:set A=a\ b`:            {"config:set", "A=a b"},
		`config:set EMPTY=""`:          {"config:set", "EMPTY="},
	} {
		got, err := splitLine(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("splitLine(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := splitLine(`config:set A="oops`); err == nil {
		t.Error("an unclosed quote must fail")
	}
}

func TestCompletions(t *testing.T) {
	root := newRoot()
	apps := []string{"blog", "shop", "shopify"}
	for _, tc := range []struct {
		line string
		want []string // nil: only check the hint
		has  []string // must be among the candidates
		hint string   // must be in the hint
	}{
		{line: "", has: []string{"apps", "config:set", "deploy", "logs", "server", "top"}},
		{line: "con", want: []string{"config", "config:get", "config:import", "config:set", "config:unset"}},
		{line: "dokwalt rollb", want: []string{"rollback"}},
		{line: "server ", has: []string{"info", "list", "upgrade"}},
		{line: "server inf", want: []string{"info"}},
		{line: "logs -a ", want: []string{"blog", "shop", "shopify"}},
		{line: "logs -a sh", want: []string{"shop", "shopify"}},
		{line: "logs --app ", want: []string{"blog", "shop", "shopify"}},
		{line: "logs --app=b", want: []string{"--app=blog"}},
		{line: "link ", want: []string{"blog", "shop", "shopify"}},
		{line: "apps:destroy s", want: []string{"shop", "shopify"}},
		{line: "apps:rename s", want: []string{"shop", "shopify"}},
		{line: "link blog ", hint: "link <app>"},
		{line: "docs ser", has: []string{"server"}},
		{line: "logs --ap", want: []string{"--app"}},
		{line: "config:set ", hint: "config:set KEY=value"},
		{line: "nope ", hint: `unknown command "nope"`},
		{line: "zz", want: []string{}, hint: `no command starts with "zz"`},
	} {
		cands, hint := completions(root, tc.line, apps)
		got := values(cands)
		if tc.want != nil && !slices.Equal(got, tc.want) && len(got)+len(tc.want) > 0 {
			t.Errorf("%q: candidates %q, want %q", tc.line, got, tc.want)
		}
		for _, h := range tc.has {
			if !slices.Contains(got, h) {
				t.Errorf("%q: %q missing from %q", tc.line, h, got)
			}
		}
		if tc.hint != "" && !strings.Contains(hint, tc.hint) {
			t.Errorf("%q: hint %q, want it to contain %q", tc.line, hint, tc.hint)
		}
	}
	// Hidden commands are not offered.
	cands, _ := completions(root, "", apps)
	if slices.Contains(values(cands), "daemon") {
		t.Error("hidden command daemon offered")
	}
}

func TestCompleteLine(t *testing.T) {
	c := func(vals ...string) []candidate {
		var out []candidate
		for _, v := range vals {
			out = append(out, candidate{value: v})
		}
		return out
	}
	for _, tc := range []struct {
		line  string
		cands []candidate
		want  string
	}{
		{"rollb", c("rollback"), "rollback "},
		{"con", c("config", "config:get", "config:set"), "config"},
		{"config:s", c("config:set"), "config:set "},
		{"logs -a sh", c("shop", "shopify"), "logs -a shop"},
		{"logs -a x", nil, "logs -a x"},
		{"", c("apps", "alerts"), "a"},
	} {
		if got := completeLine(tc.line, tc.cands); got != tc.want {
			t.Errorf("completeLine(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestCommandArgs(t *testing.T) {
	root := newRoot()
	prod := &row{app: "shop", stage: api.DefaultStage}
	staging := &row{app: "shop", stage: api.StagingStage}
	for _, tc := range []struct {
		line   string
		sel    *row
		server string
		want   string
	}{
		{"config", prod, "", "config -a shop"},
		{"dokwalt config", prod, "", "config -a shop"},
		{"config:set FOO='a b'", prod, "", "config:set -a shop FOO=a b"},
		{"releases", staging, "", "releases -a shop -s staging"},
		{"releases -s production", staging, "", "releases -a shop -s production"},
		{"logs -a blog", prod, "", "logs -a blog"},
		{"logs --app=blog", prod, "", "logs --app=blog"},
		{"exec -- ls -a", prod, "", "exec -a shop -- ls -a"},
		{"apps:import shop.json", prod, "", "apps:import shop.json"},
		{"link blog", prod, "", "link blog"},
		{"deploy -m fix", prod, "", "deploy -m fix"},
		{"apps:rename shop store", prod, "", "apps:rename shop store"},
		{"top", prod, "prod", "top --server prod"},
		{"server info", prod, "", "server info"},
		{"server info", prod, "pi", "server info --server pi"},
		{"config", nil, "", "config"},
	} {
		args, _, err := commandArgs(root, tc.line, tc.sel, tc.server)
		if err != nil {
			t.Errorf("%q: %v", tc.line, err)
			continue
		}
		if got := strings.Join(args, " "); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.line, got, tc.want)
		}
	}
	if args, _, err := commandArgs(root, "   ", prod, ""); args != nil || err != nil {
		t.Errorf("an empty line runs nothing, got %q, %v", args, err)
	}
	if _, _, err := commandArgs(root, "nope", prod, ""); err == nil {
		t.Error("an unknown command must fail before running anything")
	}
}

func TestQuoteArgs(t *testing.T) {
	if got := quoteArgs([]string{"config:set", "MSG=it's here", "A=b"}); got != `config:set 'MSG=it'\''s here' A=b` {
		t.Errorf("quoteArgs = %s", got)
	}
}

// The bar opens with ':', completes with tab, keeps history and closes with esc.
func TestCommandBarKeys(t *testing.T) {
	m := newDash(t.Context(), &session{name: "prod"})
	m.root = newRoot()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m.Update(appsMsg{
		{Name: "blog", Stages: []api.Stage{{Name: "production", Status: "running"}}},
		{Name: "shop", Pipeline: true, Stages: []api.Stage{{Name: "production", Status: "running"}, {Name: "staging", Status: "running"}}},
	})
	press := func(keys ...tea.KeyMsg) {
		for _, k := range keys {
			m.Update(k)
		}
	}
	runes := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
	press(runes(":"))
	if !m.barOn {
		t.Fatal("':' must open the command bar")
	}
	// Every command is listed before anything is typed.
	view := m.View()
	for _, c := range []string{"config:set", "rollback", "top"} {
		if !strings.Contains(view, c) {
			t.Errorf("%s not listed when the bar opens", c)
		}
	}
	press(runes("rollb"), tea.KeyMsg{Type: tea.KeyTab})
	if m.bar != "rollback " {
		t.Errorf("tab: bar = %q", m.bar)
	}
	press(runes("-a"), tea.KeyMsg{Type: tea.KeySpace}, runes("s"), tea.KeyMsg{Type: tea.KeyTab})
	if m.bar != "rollback -a shop " {
		t.Errorf("tab on an app: bar = %q", m.bar)
	}
	press(tea.KeyMsg{Type: tea.KeyCtrlW})
	if m.bar != "rollback -a " {
		t.Errorf("ctrl+w: bar = %q", m.bar)
	}
	press(tea.KeyMsg{Type: tea.KeyEsc})
	if m.barOn {
		t.Error("esc must close the bar")
	}
	m.history = []string{"config", "releases"}
	press(runes(":"), tea.KeyMsg{Type: tea.KeyUp})
	if m.bar != "releases" {
		t.Errorf("↑: bar = %q", m.bar)
	}
	press(tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown})
	if m.bar != "" {
		t.Errorf("↓ past the newest line must clear the bar, got %q", m.bar)
	}
	press(runes("nope"), tea.KeyMsg{Type: tea.KeyEnter})
	if m.barOn || !strings.Contains(m.status, "unknown command") {
		t.Errorf("an unknown command must close the bar with an error, status %q", m.status)
	}
	// While the bar is open, other keys are typed, not run.
	press(runes(":"), runes("q"))
	if !m.barOn || m.bar != "q" {
		t.Errorf("q in the bar must be typed, bar %q open %v", m.bar, m.barOn)
	}
}
