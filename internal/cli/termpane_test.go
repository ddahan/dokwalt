package cli

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestKeyBytes(t *testing.T) {
	for _, tc := range []struct {
		k    tea.KeyMsg
		want string
	}{
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ls")}, "ls"},
		{tea.KeyMsg{Type: tea.KeyEnter}, "\r"},
		{tea.KeyMsg{Type: tea.KeyBackspace}, "\x7f"},
		{tea.KeyMsg{Type: tea.KeyTab}, "\t"},
		{tea.KeyMsg{Type: tea.KeyEsc}, "\x1b"},
		{tea.KeyMsg{Type: tea.KeyCtrlC}, "\x03"},
		{tea.KeyMsg{Type: tea.KeyCtrlD}, "\x04"},
		{tea.KeyMsg{Type: tea.KeyUp}, "\x1b[A"},
		{tea.KeyMsg{Type: tea.KeyLeft}, "\x1b[D"},
		{tea.KeyMsg{Type: tea.KeySpace}, " "},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b"), Alt: true}, "\x1bb"},
	} {
		if got := keyBytes(tc.k); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.k, got, tc.want)
		}
	}
}

// waitPane collects a pane's messages until it's done.
func waitPane(t *testing.T, msgs chan tea.Msg) error {
	t.Helper()
	for {
		select {
		case m := <-msgs:
			if d, ok := m.(paneDoneMsg); ok {
				return d.err
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the command never ended")
		}
	}
}

// A command runs in a real terminal inside the pane: it gets a TTY, reads
// the keys sent to it, and its screen stays once it ends.
func TestPaneRunsACommand(t *testing.T) {
	msgs := make(chan tea.Msg, 100)
	script := `[ -t 0 ] && echo tty; printf 'name? '; read n; echo "hello $n"; exit 3`
	p, err := startPane(1, "test", []string{"sh", "-c", script}, 40, 6, false, func(m tea.Msg) { msgs <- m })
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	for _, r := range "bob" {
		p.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	p.key(tea.KeyMsg{Type: tea.KeyEnter})
	err = waitPane(t, msgs)
	p.done(err)
	time.Sleep(100 * time.Millisecond)
	screen := p.emu.String()
	for _, want := range []string{"tty", "name? bob", "hello bob"} {
		if !strings.Contains(screen, want) {
			t.Errorf("%q missing from the pane:\n%s", want, screen)
		}
	}
	if v := p.view(40); !strings.Contains(stripANSI(v), "✗ exit 3") {
		t.Errorf("exit status not shown: %s", stripANSI(v))
	}
	if p.running || p.focus {
		t.Error("a finished command keeps the keys")
	}
}

// The dashboard keeps its app list and overview while a command's pane is
// open, and the frame still fits the terminal.
func TestDashboardWithPane(t *testing.T) {
	m := newDash(t.Context(), &session{name: "prod"})
	msgs := make(chan tea.Msg, 100)
	m.send = func(msg tea.Msg) { msgs <- msg }
	m.Update(appsMsg{{Name: "blog"}})
	for _, size := range [][2]int{{120, 32}, {80, 24}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		w, h := m.paneSize()
		p, err := startPane(1, "dokwalt releases", []string{"sh", "-c", "seq 1 100"}, w, h, false, m.send)
		if err != nil {
			t.Fatal(err)
		}
		p.done(waitPane(t, msgs))
		m.pane = p
		out := m.View()
		lines := strings.Split(out, "\n")
		if len(lines) > size[1] {
			t.Errorf("%v: %d lines", size, len(lines))
		}
		for i, l := range lines {
			if w := len([]rune(stripANSI(l))); w > size[0] {
				t.Errorf("%v: line %d is %d wide", size, i, w)
			}
		}
		for _, want := range []string{"APPS", "$ dokwalt releases", "✓ done", "100"} {
			if !strings.Contains(stripANSI(out), want) {
				t.Errorf("%v: %q missing", size, want)
			}
		}
		m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if m.pane == nil || !m.pane.collapsed {
			t.Fatal("esc must shrink a finished command's output")
		}
		// Compact: the title and the last lines it printed, and the overview
		// gets the room back.
		out = stripANSI(m.View())
		if !strings.Contains(out, "$ dokwalt releases") || !strings.Contains(out, "100") || strings.Contains(out, "\n│ 50 ") {
			t.Errorf("%v: compact console:\n%s", size, out)
		}
		if n := len(strings.Split(out, "\n")); n > size[1] {
			t.Errorf("%v: %d lines once compact", size, n)
		}
		m.pane = nil
	}
	// Before any command: the console says how to run one.
	if out := stripANSI(m.View()); !strings.Contains(out, ": run a command · tab completes") {
		t.Errorf("idle console hint missing:\n%s", out)
	}
	// Numbers aren't shortcuts; ← → switch tabs.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if m.tab != tabOverview {
		t.Error("2 switched tabs")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if m.tab != tabLogs {
		t.Error("→ didn't switch to the logs tab")
	}
}
