// Package ui renders DokWalt's terminal output: a small, consistent design
// system on top of Lip Gloss, plus live progress with Bubble Tea.
package ui

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

var (
	Accent  = lipgloss.AdaptiveColor{Light: "#6D28D9", Dark: "#A78BFA"}
	Green   = lipgloss.AdaptiveColor{Light: "#047857", Dark: "#34D399"}
	Yellow  = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}
	Red     = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}
	Blue    = lipgloss.AdaptiveColor{Light: "#1D4ED8", Dark: "#60A5FA"}
	Muted   = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
	Faint   = lipgloss.AdaptiveColor{Light: "#9CA3AF", Dark: "#4B5563"}
	Text    = lipgloss.AdaptiveColor{Light: "#111827", Dark: "#F3F4F6"}
	Surface = lipgloss.AdaptiveColor{Light: "#F3F4F6", Dark: "#1F2937"}

	Bold      = lipgloss.NewStyle().Bold(true)
	TitleS    = lipgloss.NewStyle().Bold(true).Foreground(Accent)
	MutedS    = lipgloss.NewStyle().Foreground(Muted)
	FaintS    = lipgloss.NewStyle().Foreground(Faint)
	GreenS    = lipgloss.NewStyle().Foreground(Green)
	YellowS   = lipgloss.NewStyle().Foreground(Yellow)
	RedS      = lipgloss.NewStyle().Foreground(Red)
	BlueS     = lipgloss.NewStyle().Foreground(Blue)
	AccentS   = lipgloss.NewStyle().Foreground(Accent)
	CodeS     = lipgloss.NewStyle().Foreground(Blue)
	HeaderS   = lipgloss.NewStyle().Foreground(Muted).Bold(true)
	BoxS      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Faint).Padding(0, 1)
	OkIcon    = GreenS.Render("✓")
	WarnIcon  = YellowS.Render("!")
	FailIcon  = RedS.Render("✗")
	InfoIcon  = AccentS.Render("◆")
	ArrowIcon = AccentS.Render("→")
)

// Service colors for multiplexed logs.
var serviceColors = []lipgloss.AdaptiveColor{
	{Light: "#7C3AED", Dark: "#C4B5FD"}, {Light: "#0369A1", Dark: "#7DD3FC"}, {Light: "#047857", Dark: "#6EE7B7"},
	{Light: "#B45309", Dark: "#FCD34D"}, {Light: "#BE185D", Dark: "#F9A8D4"}, {Light: "#0F766E", Dark: "#5EEAD4"},
	{Light: "#4338CA", Dark: "#A5B4FC"}, {Light: "#C2410C", Dark: "#FDBA74"},
}

func ServiceColor(name string) lipgloss.Style {
	h := 0
	for _, c := range name {
		h = h*31 + int(c)
	}
	if h < 0 {
		h = -h
	}
	return lipgloss.NewStyle().Foreground(serviceColors[h%len(serviceColors)])
}

// IsTTY reports whether stdout is an interactive terminal.
func IsTTY() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

func Width() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 100
	}
	return w
}

func Title(s string) { fmt.Println(TitleS.Render("◆ " + s)) }

func Success(format string, a ...any) { fmt.Println(OkIcon + " " + fmt.Sprintf(format, a...)) }
func Warn(format string, a ...any) {
	fmt.Println(WarnIcon + " " + YellowS.Render(fmt.Sprintf(format, a...)))
}
func Info(format string, a ...any) { fmt.Println(InfoIcon + " " + fmt.Sprintf(format, a...)) }
func Hint(format string, a ...any) { fmt.Println(MutedS.Render("  " + fmt.Sprintf(format, a...))) }
func Fail(format string, a ...any) {
	fmt.Fprintln(os.Stderr, FailIcon+" "+RedS.Render(fmt.Sprintf(format, a...)))
}

// Code styles an inline command.
func Code(s string) string { return CodeS.Render(s) }

// Status renders a colored status word with a dot.
func Status(s string) string {
	switch s {
	case "running", "succeeded", "ok", "live", "enabled":
		return GreenS.Render("● " + s)
	case "deploying", "pending", "starting", "draining":
		return BlueS.Render("◌ " + s)
	case "degraded", "warn", "superseded", "stopped":
		return YellowS.Render("● " + s)
	case "not deployed", "":
		return MutedS.Render("○ not deployed")
	default:
		return RedS.Render("● " + s)
	}
}

// Table renders aligned columns without borders.
func Table(headers []string, rows [][]string) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = lipgloss.Width(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && lipgloss.Width(c) > widths[i] {
				widths[i] = lipgloss.Width(c)
			}
		}
	}
	var b strings.Builder
	line := func(cells []string, style func(string) string) {
		for i, c := range cells {
			if i >= len(widths) {
				break
			}
			pad := widths[i] - lipgloss.Width(c)
			b.WriteString(style(c))
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", pad+3))
			}
		}
		b.WriteString("\n")
	}
	line(headers, func(s string) string { return HeaderS.Render(s) })
	for _, r := range rows {
		line(r, func(s string) string { return s })
	}
	return strings.TrimRight(b.String(), "\n")
}

// KV renders key/value lines.
func KV(pairs ...string) string {
	w := 0
	for i := 0; i < len(pairs); i += 2 {
		if l := lipgloss.Width(pairs[i]); l > w {
			w = l
		}
	}
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		b.WriteString(MutedS.Render(fmt.Sprintf("%-*s", w, pairs[i])))
		b.WriteString("   ")
		b.WriteString(pairs[i+1])
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

var sparks = []rune("▁▂▃▄▅▆▇█")

// Sparkline renders values as a unicode sparkline of the given width.
func Sparkline(vals []float64, width int) string {
	if len(vals) == 0 {
		return FaintS.Render(strings.Repeat("·", width))
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	maxV := 0.0
	for _, v := range vals {
		maxV = math.Max(maxV, v)
	}
	var b strings.Builder
	for i := 0; i < width-len(vals); i++ {
		b.WriteRune(' ')
	}
	for _, v := range vals {
		i := 0
		if maxV > 0 {
			i = int(v / maxV * float64(len(sparks)-1))
		}
		b.WriteRune(sparks[i])
	}
	return AccentS.Render(b.String())
}

// Bar renders a percentage bar.
func Bar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	n := int(math.Round(pct / 100 * float64(width)))
	style := GreenS
	switch {
	case pct >= 90:
		style = RedS
	case pct >= 75:
		style = YellowS
	}
	return style.Render(strings.Repeat("━", n)) + FaintS.Render(strings.Repeat("━", width-n))
}

func Bytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func Rate(b uint64) string { return Bytes(b) + "/s" }

func Ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("2006-01-02")
}

func Duration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// Confirm asks a yes/no question (default no).
func Confirm(question string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	fmt.Print(YellowS.Render("? ") + question + MutedS.Render(" [y/N] "))
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// ConfirmName asks the user to type a name to confirm a destructive action.
func ConfirmName(action, name string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	fmt.Println(RedS.Render("! ") + action)
	fmt.Print(MutedS.Render("  Type ") + Bold.Render(name) + MutedS.Render(" to confirm: "))
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line) == name
}
