package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/ddahan/dokwalt/internal/api"
)

// Operation runs fn, rendering its events live: completed steps scroll up in
// order, the active step spins with its elapsed time, and the latest log
// lines show dimmed underneath. A single goroutine owns the terminal, so
// output order is exactly event order.
func Operation(fn func(emit func(api.Event)) (api.Event, error)) (api.Event, error) {
	if !IsTTY() {
		return fn(plainEvent)
	}
	r := newRenderer()
	go r.loop()
	ev, err := fn(r.emit)
	r.stop()
	return ev, err
}

// Spin shows a spinner while fn runs.
func Spin(label string, fn func() error) error {
	_, err := Operation(func(emit func(api.Event)) (api.Event, error) {
		emit(api.Event{Step: "spin", Status: "start", Message: label})
		err := fn()
		if err == nil {
			emit(api.Event{Step: "spin", Status: "done", Message: label})
		}
		return api.Event{}, err
	})
	return err
}

func plainEvent(e api.Event) {
	switch e.Status {
	case "start", "progress":
		fmt.Println("• " + e.Message)
	case "done":
		fmt.Println("✓ " + e.Message)
	case "warn":
		fmt.Println("! " + e.Message)
	case "log":
		prefix := "  | "
		if e.Service != "" {
			prefix = "  | " + e.Service + " "
		}
		fmt.Println(prefix + e.Message)
	}
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type renderer struct {
	mu      sync.Mutex
	pending []string // permanent lines not yet printed
	active  string
	stepAt  time.Time
	logs    []string
	drawn   int // lines of the live area currently on screen
	frame   int
	done    chan struct{}
	stopped chan struct{}
}

func newRenderer() *renderer {
	return &renderer{stepAt: time.Now(), done: make(chan struct{}), stopped: make(chan struct{})}
}

func (r *renderer) emit(e api.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch e.Status {
	case "start":
		r.active, r.stepAt, r.logs = e.Message, time.Now(), nil
	case "progress":
		if r.active == "" {
			r.stepAt = time.Now()
		}
		r.active = e.Message
	case "done":
		r.pending = append(r.pending, OkIcon+" "+e.Message+" "+FaintS.Render(Duration(time.Since(r.stepAt))))
		r.active, r.logs, r.stepAt = "", nil, time.Now()
	case "warn":
		r.pending = append(r.pending, WarnIcon+" "+YellowS.Render(e.Message))
	case "log":
		l := strings.TrimRight(e.Message, " \r")
		if e.Service != "" {
			l = ServiceColor(e.Service).Render(e.Service) + " " + l
		}
		r.logs = append(r.logs, l)
		if len(r.logs) > 6 {
			r.logs = r.logs[len(r.logs)-6:]
		}
	}
}

func (r *renderer) loop() {
	defer close(r.stopped)
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	fmt.Fprint(os.Stdout, "\x1b[?25l") // hide cursor
	defer fmt.Fprint(os.Stdout, "\x1b[?25h")
	for {
		select {
		case <-r.done:
			r.draw(true)
			return
		case <-t.C:
			r.draw(false)
		}
	}
}

func (r *renderer) stop() {
	close(r.done)
	<-r.stopped
}

// draw erases the live area, prints new permanent lines, then redraws the
// live area (unless final).
func (r *renderer) draw(final bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	if r.drawn > 0 {
		fmt.Fprintf(&b, "\x1b[%dF\x1b[J", r.drawn) // up N lines, clear to end
	}
	for _, l := range r.pending {
		b.WriteString(l + "\n")
	}
	r.pending = nil
	r.drawn = 0
	if !final {
		w := Width() - 1
		fit := func(s string) string { return lipgloss.NewStyle().MaxWidth(w).Render(s) }
		if r.active != "" {
			elapsed := time.Since(r.stepAt)
			el := fmt.Sprintf("%ds", int(elapsed.Seconds()))
			b.WriteString(fit(AccentS.Render(spinFrames[r.frame%len(spinFrames)])+" "+r.active+" "+FaintS.Render(el)) + "\n")
			r.drawn++
		}
		for _, l := range r.logs {
			b.WriteString(fit(FaintS.Render("  │ ")+MutedS.Render(l)) + "\n")
			r.drawn++
		}
		r.frame++
	}
	fmt.Fprint(os.Stdout, b.String())
}
