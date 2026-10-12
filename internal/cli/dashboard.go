package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/client"
	"github.com/ddahan/dokwalt/internal/ui"
)

// runDashboard opens the interactive full-screen dashboard.
func runDashboard(ctx context.Context, root *cobra.Command) error {
	var s *session
	connectFn := func() error {
		var err error
		s, err = connect(ctx)
		return err
	}
	var err error
	if os.Getenv("DOKWALT_SNAPSHOT") != "" {
		err = connectFn()
	} else {
		err = ui.Spin("Connecting", connectFn)
	}
	if err != nil {
		return err
	}
	defer s.Close()
	// From here on bubbletea reads ctrl+c as a key (and commands run from the
	// bar get it through their own terminal): the CLI's SIGINT handler must
	// not end the dashboard.
	ctx = context.WithoutCancel(ctx)
	m := newDash(ctx, s)
	m.root = root
	if size := os.Getenv("DOKWALT_SNAPSHOT"); size != "" {
		return m.snapshot(size)
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	m.send = p.Send
	_, err = p.Run()
	m.stopLogs()
	if m.pane != nil {
		m.pane.close()
	}
	if err == nil {
		err = m.fatal
	}
	return err
}

type dashTab int

const (
	tabOverview dashTab = iota
	tabLogs
	tabReleases
)

var tabNames = []string{"Overview", "Logs", "Releases"}

type row struct {
	app      string
	stage    string
	pipeline bool
	st       api.Stage
}

type dash struct {
	ctx    context.Context
	s      *session
	send   func(tea.Msg)
	w, h   int
	rows   []row
	sel    int
	tab    dashTab
	top    api.TopSnapshot
	svcs   []api.Service
	rels   []api.Release
	doms   []api.Domain
	logs   []string
	logKey string
	cancel context.CancelFunc
	status string // last status line
	help   bool
	fatal  error

	root    *cobra.Command // the command tree, for the command bar
	barOn   bool           // the `:` command bar is open
	bar     string         // what is typed in it
	history []string       // command bar lines run, oldest first
	histAt  int            // position while browsing history with ↑↓
	pane    *termPane      // the command run from the bar, in its own terminal
	paneSeq int
}

type (
	appsMsg    []api.App
	topSnapMsg api.TopSnapshot
	detailMsg  struct {
		key  string
		svcs []api.Service
		rels []api.Release
		doms []api.Domain
	}
	logMsg  struct{ key, line string }
	tickMsg struct{}
	errMsg  struct{ err error }
)

func newDash(ctx context.Context, s *session) *dash {
	return &dash{ctx: ctx, s: s}
}

// snapshot prints one fully loaded frame with colors, for docs and
// screenshots: DOKWALT_SNAPSHOT=120x32 dokwalt [-a app]
func (m *dash) snapshot(size string) error {
	var w, h int
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err != nil {
		return fmt.Errorf("DOKWALT_SNAPSHOT must look like 120x32")
	}
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	for _, load := range []tea.Cmd{m.loadApps(), m.loadTop()} {
		if msg := load(); msg != nil {
			if e, ok := msg.(errMsg); ok {
				return e.err
			}
			m.Update(msg)
		}
	}
	for i, r := range m.rows {
		if r.app == gf.app && (gf.stage == "" || r.stage == gf.stage) {
			m.sel = i
			break
		}
	}
	if cmd := m.loadDetail(); cmd != nil {
		m.Update(cmd())
	}
	fmt.Println(m.View())
	return nil
}

func (m *dash) cur() (row, bool) {
	if m.sel < 0 || m.sel >= len(m.rows) {
		return row{}, false
	}
	return m.rows[m.sel], true
}

func (m *dash) Init() tea.Cmd {
	return tea.Batch(m.loadApps(), m.loadTop(), tick())
}

func tick() tea.Cmd { return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} }) }

func (m *dash) loadApps() tea.Cmd {
	return func() tea.Msg {
		var apps []api.App
		if err := m.s.c.Get(m.ctx, "/v1/apps", &apps); err != nil {
			return errMsg{err}
		}
		return appsMsg(apps)
	}
}

func (m *dash) loadTop() tea.Cmd {
	return func() tea.Msg {
		var t api.TopSnapshot
		if err := m.s.c.Get(m.ctx, "/v1/top", &t); err != nil {
			return errMsg{err}
		}
		return topSnapMsg(t)
	}
}

func (m *dash) loadDetail() tea.Cmd {
	r, ok := m.cur()
	if !ok {
		return nil
	}
	key := r.app + "/" + r.stage
	return func() tea.Msg {
		d := detailMsg{key: key}
		_ = m.s.c.Get(m.ctx, client.StagePath(r.app, r.stage, "/services"), &d.svcs)
		_ = m.s.c.Get(m.ctx, client.StagePath(r.app, r.stage, "/releases?limit=30"), &d.rels)
		_ = m.s.c.Get(m.ctx, client.StagePath(r.app, r.stage, "/domains"), &d.doms)
		return d
	}
}

func (m *dash) stopLogs() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// startLogs follows the selected app's logs in the background.
func (m *dash) startLogs() {
	r, ok := m.cur()
	if !ok || m.send == nil {
		return
	}
	key := r.app + "/" + r.stage
	if m.logKey == key && m.cancel != nil {
		return
	}
	m.stopLogs()
	m.logs, m.logKey = nil, key
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	send := m.send
	go func() {
		q := url.Values{"follow": {"1"}, "tail": {"200"}}
		_ = m.s.c.Logs(ctx, client.StagePath(r.app, r.stage, "/logs"), q, func(l api.LogLine) {
			line := ui.FaintS.Render(l.Time.Local().Format("15:04:05")) + " " + ui.ServiceColor(l.Service).Render(l.Service) + " " + l.Line
			send(logMsg{key: key, line: line})
		})
	}()
}

func (m *dash) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		if m.pane != nil {
			m.pane.resize(m.paneSize())
		}
	case errMsg:
		m.status = ui.RedS.Render("✗ " + msg.err.Error())
	case appsMsg:
		prevKey := ""
		if r, ok := m.cur(); ok {
			prevKey = r.app + "/" + r.stage
		}
		m.rows = nil
		for _, a := range msg {
			for _, st := range a.Stages {
				m.rows = append(m.rows, row{app: a.Name, stage: st.Name, pipeline: a.Pipeline, st: st})
			}
		}
		for i, r := range m.rows {
			if r.app+"/"+r.stage == prevKey {
				m.sel = i
			}
		}
		if m.sel >= len(m.rows) {
			m.sel = max(0, len(m.rows)-1)
		}
		return m, m.loadDetail()
	case topSnapMsg:
		m.top = api.TopSnapshot(msg)
	case detailMsg:
		if r, ok := m.cur(); ok && r.app+"/"+r.stage == msg.key {
			m.svcs, m.rels, m.doms = msg.svcs, msg.rels, msg.doms
		}
	case logMsg:
		if msg.key == m.logKey {
			m.logs = append(m.logs, msg.line)
			if len(m.logs) > 500 {
				m.logs = m.logs[len(m.logs)-500:]
			}
		}
	case tickMsg:
		return m, tea.Batch(m.loadApps(), m.loadTop(), tick())
	case paneOutMsg:
		if m.pane != nil && m.pane.seq == msg.seq {
			m.pane.dirty.Store(false) // redrawn now
		}
	case paneDoneMsg:
		if m.pane != nil && m.pane.seq == msg.seq {
			m.pane.done(msg.err)
			return m, tea.Batch(m.loadApps(), m.loadTop(), m.loadDetail())
		}
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *dash) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if p := m.pane; p != nil && p.running {
		if k.String() == paneEscape {
			p.focus = !p.focus
			return m, nil
		}
		if p.focus {
			p.key(k)
			return m, nil
		}
	}
	if m.barOn {
		return m, m.barKey(k)
	}
	if m.help {
		m.help = false
		return m, nil
	}
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.help = true
	case ":":
		if m.pane != nil && m.pane.running {
			m.status = ui.YellowS.Render("a command is still running: " + paneEscape + " to go back to it")
			break
		}
		m.barOn, m.bar, m.histAt, m.status = true, "", len(m.history), ""
	case "esc":
		if m.pane != nil && !m.pane.running {
			m.pane.collapsed = true
		}
	case "l":
		m.tab = tabLogs
		m.startLogs()
	case "right":
		m.tab = (m.tab + 1) % 3
		if m.tab == tabLogs {
			m.startLogs()
		}
	case "left":
		m.tab = (m.tab + 2) % 3
		if m.tab == tabLogs {
			m.startLogs()
		}
	case "up", "k":
		if m.sel > 0 {
			m.sel--
			return m, m.selected()
		}
	case "down", "j":
		if m.sel < len(m.rows)-1 {
			m.sel++
			return m, m.selected()
		}
	case "o":
		for _, d := range m.doms {
			if d.RedirectTo == "" {
				openURL("https://" + d.Hostname)
				m.status = ui.MutedS.Render("opened https://" + d.Hostname)
				break
			}
		}
	}
	return m, nil
}

// barKey edits the command bar: tab completes, ↑↓ browse history, enter runs.
func (m *dash) barKey(k tea.KeyMsg) tea.Cmd {
	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.barOn = false
	case tea.KeyEnter:
		return m.runBar()
	case tea.KeyTab:
		cands, _ := completions(m.root, m.bar, m.appNames())
		m.bar = completeLine(m.bar, cands)
	case tea.KeyBackspace:
		if m.bar == "" {
			m.barOn = false
		} else {
			r := []rune(m.bar)
			m.bar = string(r[:len(r)-1])
		}
	case tea.KeyCtrlW:
		t := strings.TrimRight(m.bar, " ")
		m.bar = t[:strings.LastIndex(t, " ")+1]
	case tea.KeyCtrlU:
		m.bar = ""
	case tea.KeyUp:
		if m.histAt > 0 {
			m.histAt--
			m.bar = m.history[m.histAt]
		}
	case tea.KeyDown:
		if m.histAt < len(m.history)-1 {
			m.histAt++
			m.bar = m.history[m.histAt]
		} else {
			m.histAt, m.bar = len(m.history), ""
		}
	case tea.KeySpace:
		m.bar += " "
	case tea.KeyRunes:
		m.bar += string(k.Runes)
	}
	return nil
}

// runBar runs the command bar's line as a dokwalt command, on the whole
// terminal, then comes back to the dashboard.
func (m *dash) runBar() tea.Cmd {
	m.barOn = false
	line := strings.TrimSpace(m.bar)
	var sel *row
	if r, ok := m.cur(); ok {
		sel = &r
	}
	args, c, err := commandArgs(m.root, line, sel, gf.server)
	if err != nil {
		m.status = ui.RedS.Render("✗ " + err.Error())
		return nil
	}
	if args == nil {
		return nil
	}
	if n := len(m.history); n == 0 || m.history[n-1] != line {
		m.history = append(m.history, line)
	}
	exe, err := os.Executable()
	if err != nil {
		m.status = ui.RedS.Render("✗ " + err.Error())
		return nil
	}
	_ = c
	if m.pane != nil {
		m.pane.close()
	}
	m.paneSeq++
	w, h := m.paneSize()
	p, err := startPane(m.paneSeq, "dokwalt "+quoteArgs(args), append([]string{exe}, args...), w, h, lipgloss.HasDarkBackground(), m.send)
	if err != nil {
		m.status = ui.RedS.Render("✗ " + err.Error())
		return nil
	}
	m.pane = p
	return nil
}

// paneHeight is the command pane's share of the screen, border included: the
// app list and overview keep the rest.
func (m *dash) paneHeight() int { return max(8, (m.h-2)*55/100) }

// paneSize is the command's terminal size inside the pane.
func (m *dash) paneSize() (int, int) { return max(10, m.w-4), max(3, m.paneHeight()-3) }

// appNames lists the apps once each, for completion.
func (m *dash) appNames() []string {
	var names []string
	for i, r := range m.rows {
		if i == 0 || m.rows[i-1].app != r.app {
			names = append(names, r.app)
		}
	}
	return names
}

func (m *dash) selected() tea.Cmd {
	m.svcs, m.rels, m.doms = nil, nil, nil
	if m.tab == tabLogs {
		m.startLogs()
	}
	return m.loadDetail()
}

func openURL(u string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", u).Start()
	case "linux":
		_ = exec.Command("xdg-open", u).Start()
	}
}

// ---- view ----

func (m *dash) View() string {
	if m.w == 0 {
		return ""
	}
	header := m.header()
	footer := m.footer()
	panel := ""
	if m.barOn {
		panel = m.barPanel()
	}
	if panel == "" {
		panel = m.console()
	}
	bodyH := m.h - lipgloss.Height(header) - lipgloss.Height(footer)
	if panel != "" {
		bodyH -= lipgloss.Height(panel)
	}
	leftW := 34
	if m.w < 90 {
		leftW = 26
	}
	rightW := m.w - leftW - 1
	rb := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ui.Faint).Padding(0, 1)
	lb := rb.BorderForeground(ui.Accent)
	if m.pane != nil && m.pane.focus {
		lb = rb // the keys go to the command
	}
	left := lb.Width(leftW - 2).Height(bodyH - 2).Render(clipLines(m.appList(leftW-4, bodyH-2), leftW-4, bodyH-2))
	right := rb.Width(rightW - 2).Height(bodyH - 2).Render(clipLines(m.detail(rightW-4, bodyH-2), rightW-4, bodyH-2))
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
	parts := []string{header, body}
	if panel != "" {
		parts = append(parts, panel)
	}
	view := lipgloss.JoinVertical(lipgloss.Left, append(parts, footer)...)
	if m.help {
		return m.overlay(helpText())
	}
	return view
}

// clipLines keeps the first n lines of s once wrapped to width w.
func clipLines(s string, w, n int) string {
	lines := strings.Split(lipgloss.NewStyle().Width(w).Render(s), "\n")
	if len(lines) > n {
		lines = lines[:max(0, n)]
	}
	return strings.Join(lines, "\n")
}

func (m *dash) overlay(content string) string {
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ui.Accent).Padding(1, 3).Render(content)
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, box)
}

func helpText() string {
	keys := [][2]string{
		{"↑↓ / j k", "select an app"}, {"← →", "overview · logs · releases"}, {"l", "live logs"},
		{":", "run any dokwalt command on the selected app (tab completes),"},
		{"", "e.g. :rollback, :promote, :restart, :db:connect"},
		{paneEscape, "switch the keys between a running command and the dashboard"},
		{"esc", "shrink a finished command's output"},
		{"o", "open the site in your browser"}, {"q", "quit"},
	}
	var b strings.Builder
	b.WriteString(ui.TitleS.Render("◆ Keys") + "\n\n")
	for _, k := range keys {
		b.WriteString(fmt.Sprintf("%s  %s\n", ui.AccentS.Render(fmt.Sprintf("%-12s", k[0])), k[1]))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *dash) header() string {
	h := m.top.Host
	pct := func(u, t uint64) float64 {
		if t == 0 {
			return 0
		}
		return float64(u) / float64(t) * 100
	}
	left := ui.TitleS.Render(" ◆ DokWalt ") + ui.MutedS.Render(m.s.name)
	stats := fmt.Sprintf("%s %s %3.0f%%  %s %s %3.0f%%  %s %s %3.0f%%",
		ui.MutedS.Render("cpu"), ui.Bar(h.CPUPct, 8), h.CPUPct,
		ui.MutedS.Render("mem"), ui.Bar(pct(h.MemUsed, h.MemTotal), 8), pct(h.MemUsed, h.MemTotal),
		ui.MutedS.Render("disk"), ui.Bar(pct(h.DiskUsed, h.DiskTotal), 8), pct(h.DiskUsed, h.DiskTotal))
	if h.TempC > 0 {
		stats += ui.MutedS.Render(fmt.Sprintf("  %.0f°C", h.TempC))
	}
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(stats) - 1
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + stats
}

func (m *dash) footer() string {
	var line string
	switch {
	case m.barOn:
		line = ui.AccentS.Render(" :") + m.bar + lipgloss.NewStyle().Reverse(true).Render(" ")
	case m.pane != nil && m.pane.focus:
		line = ui.FaintS.Render(" keys go to the command · " + paneEscape + " dashboard · ctrl+c stops the command")
	case m.status != "":
		line = " " + m.status
	case m.pane != nil && m.pane.running:
		line = ui.FaintS.Render(" ↑↓ app · ← → tabs · " + paneEscape + " back to the command · ? help · q quit")
	case m.pane != nil && !m.pane.collapsed:
		line = ui.FaintS.Render(" ↑↓ app · ← → tabs · : command · esc shrink the output · ? help · q quit")
	default:
		line = ui.FaintS.Render(" ↑↓ app · ← → tabs · : command · o open · ? help · q quit")
	}
	return lipgloss.NewStyle().MaxWidth(m.w).Render(line)
}

// consoleLines is the console's height inside its frame while compact.
const consoleLines = 4

// console is the frame under the app list and overview where commands run.
// Compact when idle, with how to run one or the end of the last one's
// output; full size while a command runs, and after it until esc.
func (m *dash) console() string {
	frame := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ui.Faint).Padding(0, 1).Width(m.w - 2)
	w := m.w - 4
	switch p := m.pane; {
	case p == nil:
		return frame.Height(consoleLines).Render(ui.FaintS.Render(": run a command · tab completes"))
	case p.collapsed:
		title, lines := p.summary(w, consoleLines-1)
		return frame.Height(consoleLines).Render(title + "\n" + strings.Join(lines, "\n"))
	default:
		if p.focus {
			frame = frame.BorderForeground(ui.Accent)
		}
		_, h := m.paneSize()
		return frame.Height(h + 1).Render(clipLines(p.view(w), w, h+1))
	}
}

// barPanel shows what the command bar can complete: every matching command,
// app or flag, or the usage of the command being typed.
func (m *dash) barPanel() string {
	cands, hint := completions(m.root, m.bar, m.appNames())
	if len(cands) == 0 && hint == "" {
		return ""
	}
	w := m.w - 4
	maxRows := max(3, m.h-10) // the list matters more than the panes while typing
	clip := lipgloss.NewStyle().MaxWidth(w)
	var lines []string
	nameW, desc := 0, false
	for _, c := range cands {
		nameW = max(nameW, lipgloss.Width(c.value))
		desc = desc || c.desc != ""
	}
	switch {
	case len(cands) == 0:
		lines = append(lines, clip.Render(ui.MutedS.Render(hint)))
	case desc && len(cands) <= maxRows:
		for _, c := range cands {
			lines = append(lines, clip.Render(ui.AccentS.Render(fmt.Sprintf("%-*s", nameW, c.value))+"  "+ui.MutedS.Render(c.desc)))
		}
	default: // a grid of names, column by column
		colW := nameW + 2
		cols := max(1, w/colW)
		rows := (len(cands) + cols - 1) / cols
		more := 0
		if rows > maxRows {
			rows = maxRows - 1
			more = len(cands) - rows*cols
		}
		for r := 0; r < rows; r++ {
			var b strings.Builder
			for c := 0; c < cols; c++ {
				if i := c*rows + r; i < len(cands) && i < rows*cols {
					b.WriteString(ui.AccentS.Render(fmt.Sprintf("%-*s", colW, cands[i].value)))
				}
			}
			lines = append(lines, clip.Render(strings.TrimRight(b.String(), " ")))
		}
		if more > 0 {
			lines = append(lines, ui.MutedS.Render(fmt.Sprintf("… %d more: keep typing", more)))
		}
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ui.Faint).Padding(0, 1).
		Width(m.w - 2).Render(strings.Join(lines, "\n"))
}

func (m *dash) appList(w, h int) string {
	var b strings.Builder
	b.WriteString(ui.HeaderS.Render("APPS") + "\n")
	if len(m.rows) == 0 {
		b.WriteString(ui.MutedS.Render("\nNo apps yet.\n\n`dokwalt apps:create`"))
		return b.String()
	}
	for i, r := range m.rows {
		// Every segment of the selected row carries the highlight: an outer
		// style would be cancelled by each segment's own reset.
		seg := func(st lipgloss.Style, s string) string {
			if i == m.sel {
				st = st.Background(ui.Surface).Bold(true)
			}
			return st.Render(s)
		}
		plain := lipgloss.NewStyle()
		dotS, dot := statusDot(r.st.Status)
		left := seg(dotS, dot) + seg(plain, " "+r.app)
		if r.pipeline {
			left += seg(ui.MutedS, " "+r.stage)
		}
		right := ""
		if r.st.CurrentRelease > 0 {
			cpu := 0.0
			for _, sv := range m.top.Services {
				if sv.App == r.app && sv.Stage == r.stage {
					cpu += sv.Metrics.CPUPct
				}
			}
			right = seg(ui.MutedS, fmt.Sprintf("v%d", r.st.CurrentRelease)) + seg(plain, fmt.Sprintf(" %5.1f%%", cpu))
		}
		pad := max(1, w-lipgloss.Width(left)-lipgloss.Width(right))
		line := left + seg(plain, strings.Repeat(" ", pad)) + right
		b.WriteString(line + "\n")
		if i > h-3 {
			break
		}
	}
	return b.String()
}

func statusDot(s string) (lipgloss.Style, string) {
	switch s {
	case "running":
		return ui.GreenS, "●"
	case "deploying":
		return ui.BlueS, "◌"
	case "degraded", "stopped":
		return ui.YellowS, "●"
	case "not deployed":
		return ui.MutedS, "○"
	}
	return ui.RedS, "●"
}

func (m *dash) detail(w, h int) string {
	r, ok := m.cur()
	if !ok {
		return ui.MutedS.Render("Create an app from your project folder:\n\n  dokwalt apps:create myapp\n  dokwalt deploy")
	}
	var tabs []string
	for i, n := range tabNames {
		if dashTab(i) == m.tab {
			tabs = append(tabs, ui.AccentS.Bold(true).Underline(true).Render(n))
		} else {
			tabs = append(tabs, ui.MutedS.Render(n))
		}
	}
	title := ui.Bold.Render(label(r.app, r.stage)) + "  " + ui.Status(r.st.Status)
	head := title + "\n" + strings.Join(tabs, ui.FaintS.Render("  │  ")) + "\n\n"
	var body string
	switch m.tab {
	case tabOverview:
		body = m.overview(r, w)
	case tabLogs:
		body = m.logView(w, h-4)
	case tabReleases:
		body = m.releaseView(w, h-4)
	}
	return head + body
}

func (m *dash) overview(r row, w int) string {
	var b strings.Builder
	if len(m.doms) > 0 {
		var ds []string
		for _, d := range m.doms {
			if d.RedirectTo != "" {
				ds = append(ds, ui.MutedS.Render(d.Hostname+" → "+d.RedirectTo))
			} else {
				ds = append(ds, ui.BlueS.Render("https://"+d.Hostname))
			}
		}
		b.WriteString(strings.Join(ds, "  ") + "\n\n")
	}
	if len(m.svcs) == 0 {
		if r.st.CurrentRelease == 0 {
			b.WriteString(ui.MutedS.Render("Not deployed yet — run `dokwalt deploy` in the project folder."))
		}
		return b.String()
	}
	var rows [][]string
	for _, sv := range m.svcs {
		running, completed := 0, 0
		for _, c := range sv.Containers {
			switch {
			case c.State == "running":
				running++
			case c.State == "exited" && strings.HasPrefix(c.Status, "Exited (0)"):
				completed++ // one-shot job (e.g. migrations) that finished
			}
		}
		state := fmt.Sprintf("%d/%d", running, len(sv.Containers))
		switch {
		case completed > 0 && completed == len(sv.Containers):
			state = ui.MutedS.Render("✓ done")
		case running == len(sv.Containers) && running > 0:
			state = ui.GreenS.Render(state)
		default:
			state = ui.YellowS.Render(state)
		}
		cpu, mem := "—", "—"
		if sv.Metrics != nil {
			cpu, mem = fmt.Sprintf("%.1f%%", sv.Metrics.CPUPct), ui.Bytes(sv.Metrics.MemBytes)
		}
		kind := ui.MutedS.Render("zero-downtime")
		switch {
		case sv.Stateful:
			kind = ui.AccentS.Render("keeps data")
		case completed > 0 && completed == len(sv.Containers):
			kind = ui.MutedS.Render("runs once")
		}
		rows = append(rows, []string{ui.ServiceColor(sv.Name).Render(sv.Name), state, cpu, mem, kind})
	}
	b.WriteString(ui.Table([]string{"SERVICE", "UP", "CPU", "MEM", "ON DEPLOY"}, rows) + "\n")
	hosts := map[string]bool{}
	for _, d := range m.doms {
		hosts[d.Hostname] = true
	}
	var hrows [][]string
	for _, hm := range m.top.HTTP {
		if hosts[hm.Hostname] {
			hrows = append(hrows, []string{hm.Hostname, fmt.Sprintf("%.1f", hm.RPS), fmt.Sprintf("%.0f ms", hm.P95), fmt.Sprint(hm.S5xx)})
		}
	}
	if len(hrows) > 0 {
		sort.Slice(hrows, func(i, j int) bool { return hrows[i][0] < hrows[j][0] })
		b.WriteString("\n" + ui.Table([]string{"TRAFFIC", "REQ/S", "P95", "5XX/MIN"}, hrows) + "\n")
	}
	if spark := m.top.History[r.app+"/"+r.stage]; len(spark) > 0 {
		b.WriteString("\n" + ui.MutedS.Render("CPU, last 10 min  ") + ui.Sparkline(spark, min(len(spark), w-20)) + "\n")
	}
	return b.String()
}

func (m *dash) logView(w, h int) string {
	if len(m.logs) == 0 {
		return ui.MutedS.Render("Waiting for log lines…")
	}
	lines := m.logs
	if len(lines) > h {
		lines = lines[len(lines)-h:]
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(lipgloss.NewStyle().MaxWidth(w).Render(l) + "\n")
	}
	return b.String()
}

func (m *dash) releaseView(w, h int) string {
	if len(m.rels) == 0 {
		return ui.MutedS.Render("No releases yet.")
	}
	var b strings.Builder
	for i, r := range m.rels {
		if i >= h {
			break
		}
		mark := "  "
		if r.Current {
			mark = ui.GreenS.Render("▶ ")
		}
		desc := r.Description
		if r.Error != "" {
			desc = ui.RedS.Render(firstLine(r.Error))
		}
		line := fmt.Sprintf("%s%-5s %-11s %s %s", mark, fmt.Sprintf("v%d", r.Version), statusWord(r), ui.MutedS.Render(fmt.Sprintf("%-8s", ui.Ago(r.CreatedAt))), desc)
		line = lipgloss.NewStyle().MaxWidth(w).Render(line)
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + ui.FaintS.Render(":rollback vN to roll back"))
	return b.String()
}

func statusWord(r api.Release) string {
	s := r.Status
	if r.Current {
		s = "live"
	}
	switch s {
	case "live":
		return ui.GreenS.Render(fmt.Sprintf("%-11s", s))
	case "failed":
		return ui.RedS.Render(fmt.Sprintf("%-11s", s))
	case "deploying":
		return ui.BlueS.Render(fmt.Sprintf("%-11s", s))
	}
	return ui.MutedS.Render(fmt.Sprintf("%-11s", s))
}
