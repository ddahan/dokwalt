package cli

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/client"
	"github.com/ddahan/dokwalt/internal/ui"
)

// runDashboard opens the interactive full-screen dashboard.
func runDashboard(ctx context.Context) error {
	var s *session
	if err := ui.Spin("Connecting", func() error {
		var err error
		s, err = connect(ctx)
		return err
	}); err != nil {
		return err
	}
	defer s.Close()
	m := newDash(ctx, s)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	m.send = p.Send
	_, err := p.Run()
	m.stopLogs()
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
	ctx     context.Context
	s       *session
	send    func(tea.Msg)
	w, h    int
	rows    []row
	sel     int
	tab     dashTab
	focusR  bool
	top     api.TopSnapshot
	svcs    []api.Service
	rels    []api.Release
	relSel  int
	doms    []api.Domain
	logs    []string
	logKey  string
	cancel  context.CancelFunc
	spin    spinner.Model
	busy    string // running action label
	status  string // last status line
	confirm *pending
	help    bool
	fatal   error
}

type pending struct {
	question string
	run      func() tea.Cmd
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
	logMsg    struct{ key, line string }
	tickMsg   struct{}
	actionMsg struct {
		label string
		err   error
		ev    api.Event
	}
	progressMsg string
	errMsg      struct{ err error }
)

func newDash(ctx context.Context, s *session) *dash {
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = ui.AccentS
	return &dash{ctx: ctx, s: s, spin: sp}
}

func (m *dash) cur() (row, bool) {
	if m.sel < 0 || m.sel >= len(m.rows) {
		return row{}, false
	}
	return m.rows[m.sel], true
}

func (m *dash) Init() tea.Cmd {
	return tea.Batch(m.loadApps(), m.loadTop(), m.spin.Tick, tick())
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
			if m.relSel >= len(m.rels) {
				m.relSel = 0
			}
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
	case progressMsg:
		m.status = string(msg)
	case actionMsg:
		m.busy = ""
		if msg.err != nil {
			m.status = ui.RedS.Render("✗ " + msg.label + ": " + firstLine(msg.err.Error()))
		} else {
			m.status = ui.GreenS.Render("✓ " + msg.label + " done")
			if msg.ev.Release > 0 {
				m.status += ui.MutedS.Render(fmt.Sprintf(" — v%d is live", msg.ev.Release))
			}
		}
		return m, tea.Batch(m.loadApps(), m.loadDetail())
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *dash) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.confirm != nil {
		switch k.String() {
		case "y", "Y":
			p := m.confirm
			m.confirm = nil
			return m, p.run()
		default:
			m.confirm = nil
			m.status = ui.MutedS.Render("cancelled")
		}
		return m, nil
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
	case "tab":
		m.focusR = !m.focusR
	case "1", "2", "3":
		m.tab = dashTab(k.String()[0] - '1')
		if m.tab == tabLogs {
			m.startLogs()
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
		if m.focusR && m.tab == tabReleases {
			m.relSel = max(0, m.relSel-1)
		} else if m.sel > 0 {
			m.sel--
			return m, m.selected()
		}
	case "down", "j":
		if m.focusR && m.tab == tabReleases {
			m.relSel = min(len(m.rels)-1, m.relSel+1)
		} else if m.sel < len(m.rows)-1 {
			m.sel++
			return m, m.selected()
		}
	case "R":
		if r, ok := m.cur(); ok {
			m.ask(fmt.Sprintf("Restart %s?", label(r.app, r.stage)), func() tea.Cmd {
				return m.action("Restart "+label(r.app, r.stage), func(emit func(api.Event)) (api.Event, error) {
					return api.Event{}, m.s.c.Post(m.ctx, client.StagePath(r.app, r.stage, "/restart"), nil, nil)
				})
			})
		}
	case "r":
		r, ok := m.cur()
		if !ok || r.st.CurrentRelease == 0 {
			break
		}
		version, what := 0, "the previous release"
		if m.tab == tabReleases && m.relSel < len(m.rels) && !m.rels[m.relSel].Current {
			version = m.rels[m.relSel].Version
			what = fmt.Sprintf("v%d", version)
		}
		m.ask(fmt.Sprintf("Roll back %s to %s?", label(r.app, r.stage), what), func() tea.Cmd {
			return m.action("Rollback "+label(r.app, r.stage), func(emit func(api.Event)) (api.Event, error) {
				return m.s.c.Operation(m.ctx, "POST", client.StagePath(r.app, r.stage, "/rollback"), api.RollbackRequest{Version: version}, emit)
			})
		})
	case "p":
		r, ok := m.cur()
		if !ok || !r.pipeline {
			m.status = ui.MutedS.Render("promote needs a pipeline (dokwalt pipeline:enable)")
			break
		}
		m.ask(fmt.Sprintf("Promote %s staging → production?", r.app), func() tea.Cmd {
			return m.action("Promote "+r.app, func(emit func(api.Event)) (api.Event, error) {
				return m.s.c.Operation(m.ctx, "POST", "/v1/apps/"+r.app+"/promote", nil, emit)
			})
		})
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

func (m *dash) selected() tea.Cmd {
	m.svcs, m.rels, m.doms, m.relSel = nil, nil, nil, 0
	if m.tab == tabLogs {
		m.startLogs()
	}
	return m.loadDetail()
}

func (m *dash) ask(q string, run func() tea.Cmd) {
	if m.busy != "" {
		m.status = ui.YellowS.Render("wait: " + m.busy + " is running")
		return
	}
	m.confirm = &pending{question: q, run: run}
}

func (m *dash) action(label string, fn func(emit func(api.Event)) (api.Event, error)) tea.Cmd {
	m.busy, m.status = label, ""
	send := m.send
	return func() tea.Msg {
		ev, err := fn(func(e api.Event) {
			if e.Status != "log" && send != nil {
				send(progressMsg(ui.MutedS.Render(e.Message)))
			}
		})
		return actionMsg{label: label, err: err, ev: ev}
	}
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
	bodyH := m.h - lipgloss.Height(header) - lipgloss.Height(footer)
	leftW := 34
	if m.w < 90 {
		leftW = 26
	}
	rightW := m.w - leftW - 1
	border := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ui.Faint).Padding(0, 1)
	focus := border.BorderForeground(ui.Accent)
	lb, rb := focus, border
	if m.focusR {
		lb, rb = border, focus
	}
	left := lb.Width(leftW - 2).Height(bodyH - 2).Render(m.appList(leftW-4, bodyH-2))
	right := rb.Width(rightW - 2).Height(bodyH - 2).Render(m.detail(rightW-4, bodyH-2))
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
	view := lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
	if m.help {
		return m.overlay(helpText())
	}
	if m.confirm != nil {
		return m.overlay(ui.YellowS.Render("? ") + m.confirm.question + "\n\n" + ui.MutedS.Render("y confirm · any other key cancels"))
	}
	return view
}

func (m *dash) overlay(content string) string {
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ui.Accent).Padding(1, 3).Render(content)
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, box)
}

func helpText() string {
	keys := [][2]string{
		{"↑↓ / j k", "select app (or release when focused)"}, {"tab", "focus list / detail"},
		{"1 2 3 / ← →", "overview · logs · releases"}, {"l", "live logs"},
		{"r", "roll back (selected release in Releases)"}, {"p", "promote staging → production"},
		{"R", "restart"}, {"o", "open the site in your browser"}, {"q", "quit"},
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
	case m.busy != "":
		line = " " + m.spin.View() + " " + m.busy + "  " + m.status
	case m.status != "":
		line = " " + m.status
	default:
		line = ui.FaintS.Render(" ↑↓ select · 1/2/3 tabs · r rollback · p promote · R restart · o open · ? help · q quit")
	}
	return lipgloss.NewStyle().MaxWidth(m.w).Render(line)
}

func (m *dash) appList(w, h int) string {
	var b strings.Builder
	b.WriteString(ui.HeaderS.Render("APPS") + "\n")
	if len(m.rows) == 0 {
		b.WriteString(ui.MutedS.Render("\nNo apps yet.\n\n`dokwalt apps:create`"))
		return b.String()
	}
	for i, r := range m.rows {
		dot := statusDot(r.st.Status)
		name := r.app
		if r.pipeline {
			name += ui.MutedS.Render(" " + r.stage)
		}
		spark := ui.Sparkline(m.top.History[r.app+"/"+r.stage], 8)
		rel := ""
		if r.st.CurrentRelease > 0 {
			rel = ui.MutedS.Render(fmt.Sprintf("v%d", r.st.CurrentRelease))
		}
		line := fmt.Sprintf("%s %s", dot, name)
		pad := w - lipgloss.Width(line) - lipgloss.Width(rel) - 10
		if pad < 1 {
			pad = 1
		}
		line += strings.Repeat(" ", pad) + rel + " " + spark
		if i == m.sel {
			line = lipgloss.NewStyle().Background(ui.Surface).Bold(true).Width(w).Render(line)
		}
		b.WriteString(line + "\n")
		if i > h-3 {
			break
		}
	}
	return b.String()
}

func statusDot(s string) string {
	switch s {
	case "running":
		return ui.GreenS.Render("●")
	case "deploying":
		return ui.BlueS.Render("◌")
	case "degraded", "stopped":
		return ui.YellowS.Render("●")
	case "not deployed":
		return ui.MutedS.Render("○")
	}
	return ui.RedS.Render("●")
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
		running := 0
		for _, c := range sv.Containers {
			if c.State == "running" {
				running++
			}
		}
		state := fmt.Sprintf("%d/%d", running, len(sv.Containers))
		if running == len(sv.Containers) && running > 0 {
			state = ui.GreenS.Render(state)
		} else {
			state = ui.YellowS.Render(state)
		}
		cpu, mem := "—", "—"
		if sv.Metrics != nil {
			cpu, mem = fmt.Sprintf("%.1f%%", sv.Metrics.CPUPct), ui.Bytes(sv.Metrics.MemBytes)
		}
		kind := ui.MutedS.Render("blue/green")
		if sv.Stateful {
			kind = ui.AccentS.Render("stateful")
		}
		rows = append(rows, []string{ui.ServiceColor(sv.Name).Render(sv.Name), state, cpu, mem, kind})
	}
	b.WriteString(ui.Table([]string{"SERVICE", "UP", "CPU", "MEM", "MODE"}, rows) + "\n")
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
		if i == m.relSel && m.focusR {
			line = lipgloss.NewStyle().Background(ui.Surface).Width(w).Render(line)
		}
		b.WriteString(line + "\n")
	}
	if m.focusR {
		b.WriteString("\n" + ui.FaintS.Render("r roll back to the selected release"))
	} else {
		b.WriteString("\n" + ui.FaintS.Render("tab to select a release"))
	}
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
