package cli

import (
	"errors"
	"fmt"
	"image/color"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"github.com/ddahan/dokwalt/internal/ui"
)

// termPane runs a command in a terminal of its own, drawn inside the
// dashboard: a PTY for the command, a terminal emulator for its screen. So
// everything works there, from `releases` to psql, `exec -- sh` or `top`,
// while the app list and overview stay on screen.
type termPane struct {
	seq     int // identifies its messages; a new command gets a new pane
	title   string
	emu     *vt.SafeEmulator
	pty     *os.File
	cmd     *exec.Cmd
	start   time.Time
	running bool
	focus   bool // keys go to the command
	err     error
	took    time.Duration
	dirty   atomic.Bool // a redraw is already queued
}

type (
	paneOutMsg  struct{ seq int }
	paneDoneMsg struct {
		seq int
		err error
	}
)

// paneEscape gives the keys back to the dashboard (and to the command again):
// the telnet escape, which shells and database clients don't use.
const paneEscape = "ctrl+]"

// startPane runs argv in a new pane of w×h cells.
func startPane(seq int, title string, argv []string, w, h int, dark bool, send func(tea.Msg)) (*termPane, error) {
	p := &termPane{seq: seq, title: title, emu: vt.NewSafeEmulator(w, h), start: time.Now(), running: true, focus: true}
	// The command asks for the background color to pick its palette: the
	// dashboard's own.
	if dark {
		p.emu.SetDefaultBackgroundColor(color.Black)
		p.emu.SetDefaultForegroundColor(color.White)
	} else {
		p.emu.SetDefaultBackgroundColor(color.White)
		p.emu.SetDefaultForegroundColor(color.Black)
	}
	p.cmd = exec.Command(argv[0], argv[1:]...)
	p.cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(p.cmd, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
	if err != nil {
		return nil, err
	}
	p.pty = f
	go func() { _, _ = io.Copy(f, p.emu) }() // the emulator's answers to terminal queries
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				_, _ = p.emu.Write(buf[:n])
				if p.dirty.CompareAndSwap(false, true) {
					send(paneOutMsg{seq})
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		err := p.cmd.Wait()
		send(paneDoneMsg{seq, err})
	}()
	return p, nil
}

// resize follows the dashboard's size.
func (p *termPane) resize(w, h int) {
	if w == p.emu.Width() && h == p.emu.Height() {
		return
	}
	p.emu.Resize(w, h)
	if p.running {
		_ = pty.Setsize(p.pty, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
	}
}

// done records the end of the command; its screen stays to be read.
func (p *termPane) done(err error) {
	p.running, p.focus, p.err, p.took = false, false, err, time.Since(p.start)
	_ = p.pty.Close()
	_ = p.emu.Close()
}

// close stops the command if it still runs.
func (p *termPane) close() {
	if p.running && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.pty.Close()
}

// key sends a key to the command, as a terminal would encode it.
func (p *termPane) key(k tea.KeyMsg) {
	if b := keyBytes(k); b != "" {
		_, _ = io.WriteString(p.pty, b)
	}
}

func keyBytes(k tea.KeyMsg) string {
	var s string
	switch k.Type {
	case tea.KeyRunes:
		s = string(k.Runes)
	case tea.KeySpace:
		s = " "
	case tea.KeyUp:
		s = "\x1b[A"
	case tea.KeyDown:
		s = "\x1b[B"
	case tea.KeyRight:
		s = "\x1b[C"
	case tea.KeyLeft:
		s = "\x1b[D"
	case tea.KeyHome:
		s = "\x1b[H"
	case tea.KeyEnd:
		s = "\x1b[F"
	case tea.KeyPgUp:
		s = "\x1b[5~"
	case tea.KeyPgDown:
		s = "\x1b[6~"
	case tea.KeyDelete:
		s = "\x1b[3~"
	case tea.KeyInsert:
		s = "\x1b[2~"
	case tea.KeyShiftTab:
		s = "\x1b[Z"
	default:
		// Enter, tab, backspace, esc and ctrl+letter are their control codes.
		if k.Type >= 0 && k.Type <= 31 || k.Type == 127 {
			s = string(rune(k.Type))
		}
	}
	if k.Alt && s != "" {
		s = "\x1b" + s
	}
	return s
}

// view draws the pane: a title line with the command and its state, then its
// screen, with the cursor while it has the keys.
func (p *termPane) view(w int) string {
	state := ui.AccentS.Render("● running") + ui.FaintS.Render("  "+paneEscape+" dashboard")
	switch {
	case p.running && !p.focus:
		state = ui.AccentS.Render("● running") + ui.FaintS.Render("  "+paneEscape+" back to it")
	case !p.running && p.err == nil:
		state = ui.GreenS.Render("✓ done") + ui.FaintS.Render(" "+ui.Duration(p.took)+"  esc close")
	case !p.running:
		state = ui.RedS.Render("✗ "+exitText(p.err)) + ui.FaintS.Render(" "+ui.Duration(p.took)+"  esc close")
	}
	title := ui.AccentS.Render("$ ") + p.title
	if gap := w - lipgloss.Width(title) - lipgloss.Width(state); gap > 0 {
		title += strings.Repeat(" ", gap) + state
	} else {
		title = ansi.Truncate(title, max(0, w-lipgloss.Width(state)-1), "…") + " " + state
	}
	lines := strings.Split(p.emu.Render(), "\n")
	if p.running && p.focus && !p.emu.CursorHidden() {
		c := p.emu.CursorPosition()
		if c.Y >= 0 && c.Y < len(lines) {
			lines[c.Y] = withCursor(lines[c.Y], c.X)
		}
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "")
	}
	return title + "\n" + strings.Join(lines, "\n")
}

// withCursor draws a block cursor at column x of a rendered line.
func withCursor(line string, x int) string {
	ch := ansi.Strip(ansi.Cut(line, x, x+1))
	if ch == "" {
		ch = " "
	}
	left := ansi.Truncate(line, x, "")
	if pad := x - ansi.StringWidth(left); pad > 0 {
		left += strings.Repeat(" ", pad)
	}
	return left + "\x1b[0m" + lipgloss.NewStyle().Reverse(true).Render(ch) + ansi.TruncateLeft(line, x+1, "")
}

func exitText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("exit %d", ee.ExitCode())
	}
	return firstLine(err.Error())
}
