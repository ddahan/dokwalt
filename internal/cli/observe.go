package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/client"
	"github.com/ddahan/dokwalt/internal/ui"
)

func observeCommands() []*cobra.Command {
	return []*cobra.Command{logsCmd(), execCmd(), dbConnectCmd(), topCmd(), metricsCmd(),
		alertsCmd(), alertsAddCmd(), alertsRemoveCmd(), alertsTestCmd(), alertsSetCmd(), doctorCmd(), docsCmd()}
}

func logsCmd() *cobra.Command {
	var (
		follow  bool
		service string
		grep    string
		tail    int
		since   string
	)
	cmd := &cobra.Command{
		Use: "logs", Short: "Show (and follow) logs of all services, merged",
		Example: "  dokwalt logs -f\n  dokwalt logs --service web --since 1h --grep error",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			q := url.Values{"tail": {strconv.Itoa(tail)}}
			if follow {
				q.Set("follow", "1")
			}
			if service != "" {
				q.Set("service", service)
			}
			if grep != "" {
				q.Set("grep", grep)
			}
			if since != "" {
				q.Set("since", since)
			}
			// Align service names from the start.
			width := 0
			var svcs []api.Service
			if s.c.Get(cmd.Context(), client.StagePath(app, stage, "/services"), &svcs) == nil {
				for _, sv := range svcs {
					width = max(width, len(sv.Name))
				}
			}
			return s.c.Logs(cmd.Context(), client.StagePath(app, stage, "/logs"), q, func(l api.LogLine) {
				if gf.json {
					_ = printJSONLine(l)
					return
				}
				name := l.Service
				if l.Replica > 1 {
					name = fmt.Sprintf("%s.%d", l.Service, l.Replica)
				}
				if len(name) > width {
					width = len(name)
				}
				ts := ui.FaintS.Render(l.Time.Local().Format("15:04:05"))
				tag := ui.ServiceColor(l.Service).Render(fmt.Sprintf("%-*s", width, name))
				line := l.Line
				if grep != "" {
					line = highlight(line, grep)
				}
				fmt.Printf("%s %s %s %s\n", ts, tag, ui.FaintS.Render("│"), line)
			})
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new lines")
	cmd.Flags().StringVar(&service, "service", "", "only this service")
	cmd.Flags().StringVar(&grep, "grep", "", "only lines containing this text (case-insensitive)")
	cmd.Flags().IntVarP(&tail, "lines", "n", 100, "number of past lines")
	cmd.Flags().StringVar(&since, "since", "", "lines since a duration ago, e.g. 30m, 2h")
	return cmd
}

func printJSONLine(v any) error {
	b, err := jsonMarshal(v)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func highlight(line, needle string) string {
	i := strings.Index(strings.ToLower(line), strings.ToLower(needle))
	if i < 0 {
		return line
	}
	return line[:i] + ui.YellowS.Bold(true).Render(line[i:i+len(needle)]) + line[i+len(needle):]
}

func execCmd() *cobra.Command {
	var service string
	cmd := &cobra.Command{
		Use: "exec [--service web] -- <command>", Short: "Run a command inside a running container",
		Example: "  dokwalt exec -- sh\n  dokwalt exec --service web -- python manage.py migrate",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				args = []string{"sh"}
			}
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var t api.ExecTarget
			if err := s.c.Get(cmd.Context(), client.StagePath(app, stage, "/exec?service="+url.QueryEscape(service)), &t); err != nil {
				return err
			}
			quoted := make([]string, len(args))
			for i, a := range args {
				quoted[i] = shellQuote(a)
			}
			flags := "-i"
			if ui.IsTTY() {
				flags = "-it"
			}
			remote := fmt.Sprintf("docker exec %s %s %s", flags, t.Container, strings.Join(quoted, " "))
			if s.ssh == nil {
				c := exec.CommandContext(cmd.Context(), "sh", "-c", remote)
				c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
				return c.Run()
			}
			return s.ssh.Interactive(remote)
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "service (default: the first stateless one)")
	return cmd
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func dbConnectCmd() *cobra.Command {
	var (
		service    string
		tunnelOnly bool
		port       int
		remotePort int
	)
	cmd := &cobra.Command{
		Use: "db:connect", Short: "Open a database shell (or a tunnel for a GUI) over SSH",
		Long:    "Open a secure tunnel to a database service through SSH — nothing is exposed publicly — and launch psql, mysql, mongosh or redis-cli. With --tunnel-only, print a local URL for your GUI tool instead.",
		Example: "  dokwalt db:connect\n  dokwalt db:connect --service db --tunnel-only --port 5433",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			q := url.Values{}
			if service != "" {
				q.Set("service", service)
			}
			if remotePort > 0 {
				q.Set("port", strconv.Itoa(remotePort))
			}
			var t api.DBTarget
			if err := s.c.Get(cmd.Context(), client.StagePath(app, stage, "/db?"+q.Encode()), &t); err != nil {
				return err
			}
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return err
			}
			defer ln.Close()
			local := ln.Addr().(*net.TCPAddr).Port
			go serveTunnel(cmd.Context(), ln, s.c, t.Address)

			dbURL := dbURL(t, local)
			tool, toolArgs, env := dbClient(t, local)
			if !tunnelOnly {
				if _, err := exec.LookPath(tool); err != nil {
					ui.Warn("%s not found on this machine — keeping the tunnel open instead", tool)
					tunnelOnly = true
				}
			}
			if tunnelOnly || t.Kind == "unknown" {
				fmt.Println(ui.TitleS.Render("◆ Tunnel to " + t.Service + " (" + t.Kind + ")"))
				fmt.Println(ui.KV("Local address", fmt.Sprintf("127.0.0.1:%d", local), "URL", ui.Bold.Render(dbURL)))
				ui.Hint("Press Ctrl+C to close the tunnel")
				<-cmd.Context().Done()
				return nil
			}
			ui.Info("Connected to %s (%s) through SSH — exit the shell to close the tunnel", ui.Bold.Render(t.Service), t.Kind)
			c := exec.CommandContext(cmd.Context(), tool, toolArgs...)
			c.Env = append(os.Environ(), env...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "database service (default: detected)")
	cmd.Flags().BoolVar(&tunnelOnly, "tunnel-only", false, "only open the tunnel and print its URL")
	cmd.Flags().IntVar(&port, "port", 0, "local port (default: random free port)")
	cmd.Flags().IntVar(&remotePort, "remote-port", 0, "database port in the container (default: detected)")
	return cmd
}

func serveTunnel(ctx context.Context, ln net.Listener, c *client.Client, addr string) {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			remote, err := c.Tunnel(addr)
			if err != nil {
				ui.Fail("tunnel: %v", err)
				return
			}
			defer remote.Close()
			// When the local client half-closes, forward the EOF and keep
			// reading: the server's reply may still be on its way.
			go func() { _, _ = io.Copy(remote, conn); closeWrite(remote) }()
			_, _ = io.Copy(conn, remote)
		}()
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func dbURL(t api.DBTarget, port int) string {
	userinfo := ""
	if t.User != "" {
		userinfo = url.UserPassword(t.User, t.Password).String() + "@"
		if t.Password == "" {
			userinfo = url.User(t.User).String() + "@"
		}
	}
	switch t.Kind {
	case "postgres":
		return fmt.Sprintf("postgres://%s127.0.0.1:%d/%s", userinfo, port, t.Database)
	case "mysql":
		return fmt.Sprintf("mysql://%s127.0.0.1:%d/%s", userinfo, port, t.Database)
	case "mongo":
		return fmt.Sprintf("mongodb://%s127.0.0.1:%d/%s?authSource=admin", userinfo, port, t.Database)
	case "redis":
		if t.Password != "" {
			return fmt.Sprintf("redis://:%s@127.0.0.1:%d", url.PathEscape(t.Password), port)
		}
		return fmt.Sprintf("redis://127.0.0.1:%d", port)
	}
	return fmt.Sprintf("tcp://127.0.0.1:%d", port)
}

// dbClient builds the local client command; passwords go through env vars
// rather than arguments (visible in `ps`).
func dbClient(t api.DBTarget, port int) (string, []string, []string) {
	p := strconv.Itoa(port)
	switch t.Kind {
	case "postgres":
		return "psql", []string{"-h", "127.0.0.1", "-p", p, "-U", t.User, t.Database}, []string{"PGPASSWORD=" + t.Password}
	case "mysql":
		args := []string{"-h", "127.0.0.1", "-P", p, "-u", t.User}
		if t.Database != "" {
			args = append(args, t.Database)
		}
		return "mysql", args, []string{"MYSQL_PWD=" + t.Password}
	case "mongo":
		return "mongosh", []string{dbURL(t, port)}, nil
	case "redis":
		return "redis-cli", []string{"-h", "127.0.0.1", "-p", p}, []string{"REDISCLI_AUTH=" + t.Password}
	}
	return "", nil, nil
}

// ---- top ----

func topCmd() *cobra.Command {
	return &cobra.Command{
		Use: "top", Short: "Live CPU, memory, network and traffic of every app",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			fetch := func() (api.TopSnapshot, error) {
				var snap api.TopSnapshot
				err := s.c.Get(cmd.Context(), "/v1/top", &snap)
				return snap, err
			}
			if gf.json || !ui.IsTTY() {
				snap, err := fetch()
				if err != nil {
					return err
				}
				if gf.json {
					return printJSON(snap)
				}
				fmt.Println(renderTop(snap, s.name, 100))
				return nil
			}
			m := &topModel{fetch: fetch, server: s.name}
			_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
			if err == nil {
				err = m.err
			}
			return err
		},
	}
}

type topMsg struct {
	snap api.TopSnapshot
	err  error
}

type topModel struct {
	fetch  func() (api.TopSnapshot, error)
	server string
	snap   api.TopSnapshot
	err    error
	width  int
	loaded bool
}

func (m *topModel) load() tea.Cmd {
	return func() tea.Msg {
		snap, err := m.fetch()
		return topMsg{snap, err}
	}
}

func (m *topModel) Init() tea.Cmd { return m.load() }

func (m *topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case topMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, tea.Quit
		}
		m.snap, m.loaded = msg.snap, true
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return m.load()() })
	}
	return m, nil
}

func (m *topModel) View() string {
	if !m.loaded {
		return ui.MutedS.Render("  Loading…")
	}
	return renderTop(m.snap, m.server, m.width) + "\n\n" + ui.FaintS.Render("  q quit · refreshes every 2s")
}

func renderTop(snap api.TopSnapshot, server string, width int) string {
	h := snap.Host
	var b strings.Builder
	b.WriteString(ui.TitleS.Render("◆ "+server) + ui.MutedS.Render("  "+snap.Time.Local().Format("15:04:05")) + "\n\n")
	pct := func(u, t uint64) float64 {
		if t == 0 {
			return 0
		}
		return float64(u) / float64(t) * 100
	}
	bar := 24
	lines := []string{
		fmt.Sprintf("%s %s %5.1f%%  %s", ui.MutedS.Render("CPU "), ui.Bar(h.CPUPct, bar), h.CPUPct, ui.MutedS.Render(fmt.Sprintf("load %.2f · %d cores", h.Load1, h.CPUs))),
		fmt.Sprintf("%s %s %5.1f%%  %s", ui.MutedS.Render("MEM "), ui.Bar(pct(h.MemUsed, h.MemTotal), bar), pct(h.MemUsed, h.MemTotal), ui.MutedS.Render(ui.Bytes(h.MemUsed)+" / "+ui.Bytes(h.MemTotal))),
		fmt.Sprintf("%s %s %5.1f%%  %s", ui.MutedS.Render("DISK"), ui.Bar(pct(h.DiskUsed, h.DiskTotal), bar), pct(h.DiskUsed, h.DiskTotal), ui.MutedS.Render(ui.Bytes(h.DiskUsed)+" / "+ui.Bytes(h.DiskTotal))),
	}
	if h.TempC > 0 {
		t := fmt.Sprintf("%.0f°C", h.TempC)
		if h.TempC >= 75 {
			t = ui.YellowS.Render(t)
		}
		extra := ""
		if h.Throttled != "" {
			extra = "  " + ui.RedS.Render(h.Throttled)
		}
		lines = append(lines, ui.MutedS.Render("TEMP ")+t+extra)
	}
	b.WriteString(strings.Join(lines, "\n") + "\n\n")

	var rows [][]string
	lastApp := ""
	for _, sv := range snap.Services {
		appCol := ""
		key := sv.App + "/" + sv.Stage
		if key != lastApp {
			appCol = ui.Bold.Render(sv.App)
			if sv.Stage != api.DefaultStage {
				appCol += ui.MutedS.Render(" " + sv.Stage)
			}
			lastApp = key
		}
		spark := ""
		if appCol != "" {
			spark = ui.Sparkline(snap.History[key], 20)
		}
		mem := ui.Bytes(sv.Metrics.MemBytes)
		if sv.Metrics.MemLimit > 0 {
			mem += ui.MutedS.Render(" / " + ui.Bytes(sv.Metrics.MemLimit))
		}
		rows = append(rows, []string{appCol, ui.ServiceColor(sv.Service).Render(sv.Service) + ui.MutedS.Render(fmt.Sprintf(" ×%d", sv.Replicas)),
			fmt.Sprintf("%5.1f%%", sv.Metrics.CPUPct), mem,
			ui.MutedS.Render("↓") + ui.Rate(sv.Metrics.NetRx) + " " + ui.MutedS.Render("↑") + ui.Rate(sv.Metrics.NetTx), spark})
	}
	if len(rows) == 0 {
		b.WriteString(ui.MutedS.Render("  No running apps.\n"))
	} else {
		b.WriteString(ui.Table([]string{"APP", "SERVICE", "CPU", "MEMORY", "NETWORK", "CPU (10 MIN)"}, rows) + "\n")
	}
	if len(snap.HTTP) > 0 {
		b.WriteString("\n")
		var hrows [][]string
		for _, hm := range snap.HTTP {
			errs := fmt.Sprint(hm.S5xx)
			if hm.S5xx > 0 {
				errs = ui.RedS.Render(errs)
			}
			hrows = append(hrows, []string{ui.Bold.Render(hm.Hostname), fmt.Sprintf("%.1f", hm.RPS), fmt.Sprint(hm.Requests), fmt.Sprintf("%.0f ms", hm.P50), fmt.Sprintf("%.0f ms", hm.P95), errs})
		}
		b.WriteString(ui.Table([]string{"DOMAIN", "REQ/S", "REQ (LAST MIN)", "P50", "P95", "5XX"}, hrows))
	}
	_ = width
	return lipgloss.NewStyle().PaddingLeft(1).Render(b.String())
}

// ---- metrics history ----

func metricsCmd() *cobra.Command {
	var since string
	cmd := &cobra.Command{
		Use: "metrics", Short: "Metrics history (CPU, memory, requests, latency) as sparklines",
		Example: "  dokwalt metrics            # this app, last 24h\n  dokwalt metrics --since 7d",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if n, err := strconv.Atoi(strings.TrimSuffix(since, "d")); err == nil && strings.HasSuffix(since, "d") {
				since = strconv.Itoa(n*24) + "h"
			}
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var h api.MetricsHistory
			app, aerr := appName(s.cfg)
			if aerr == nil {
				err = s.c.Get(cmd.Context(), client.StagePath(app, gf.stage, "/metrics?since="+url.QueryEscape(since)), &h)
			} else {
				err = s.c.Get(cmd.Context(), "/v1/metrics?since="+url.QueryEscape(since), &h)
			}
			if err != nil {
				return err
			}
			if gf.json {
				return printJSON(h)
			}
			title := s.name
			if aerr == nil {
				title = label(app, gf.stage)
			}
			fmt.Println(ui.TitleS.Render("◆ "+title) + ui.MutedS.Render("  last "+since))
			w := 48
			series := func(name string, vals []float64, format func(float64) string) {
				if len(vals) == 0 {
					return
				}
				last, maxV := vals[len(vals)-1], 0.0
				for _, v := range vals {
					if v > maxV {
						maxV = v
					}
				}
				fmt.Printf("  %-28s %s  %s\n", name, ui.Sparkline(vals, w), ui.MutedS.Render("now "+format(last)+" · max "+format(maxV)))
			}
			pctF := func(v float64) string { return fmt.Sprintf("%.1f%%", v) }
			bytesF := func(v float64) string { return ui.Bytes(uint64(v)) }
			intF := func(v float64) string { return fmt.Sprintf("%.0f", v) }
			msF := func(v float64) string { return fmt.Sprintf("%.0fms", v) }
			if len(h.Host) > 0 && aerr != nil {
				var cpu, mem []float64
				for _, p := range h.Host {
					cpu, mem = append(cpu, p.CPUPct), append(mem, float64(p.MemBytes))
				}
				fmt.Println()
				series("host CPU", cpu, pctF)
				series("host memory", mem, bytesF)
			}
			names := make([]string, 0, len(h.Services))
			for n := range h.Services {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				var cpu, mem []float64
				for _, p := range h.Services[n] {
					cpu, mem = append(cpu, p.CPUPct), append(mem, float64(p.MemBytes))
				}
				fmt.Println()
				fmt.Println("  " + ui.ServiceColor(n).Render(n))
				series("  CPU", cpu, pctF)
				series("  memory", mem, bytesF)
			}
			hosts := make([]string, 0, len(h.HTTP))
			for n := range h.HTTP {
				hosts = append(hosts, n)
			}
			sort.Strings(hosts)
			for _, n := range hosts {
				var req, p95, errs []float64
				for _, p := range h.HTTP[n] {
					req, p95, errs = append(req, float64(p.Requests)), append(p95, p.P95), append(errs, float64(p.S5xx))
				}
				fmt.Println()
				fmt.Println("  " + ui.Bold.Render(n))
				series("  requests / bucket", req, intF)
				series("  p95 latency", p95, msF)
				series("  5xx errors", errs, intF)
			}
			if len(names) == 0 && len(hosts) == 0 && len(h.Host) == 0 {
				ui.Info("No data yet — metrics are aggregated every minute.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "24h", "time range (e.g. 1h, 24h, 7d)")
	return cmd
}

// ---- alerts ----

func alertsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "alerts", Short: "Show alert channels, thresholds and what is firing",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var a api.AlertsConfig
			if err := s.c.Get(cmd.Context(), "/v1/alerts", &a); err != nil {
				return err
			}
			if gf.json {
				return printJSON(a)
			}
			fmt.Println(ui.TitleS.Render("◆ Alerts on " + s.name))
			fmt.Println()
			if len(a.Channels) == 0 {
				ui.Warn("No channel: alerts are only logged on the server")
				ui.Hint("Add one: %s", ui.Code("dokwalt alerts:add discord https://discord.com/api/webhooks/…"))
			} else {
				var rows [][]string
				for _, c := range a.Channels {
					rows = append(rows, []string{fmt.Sprint(c.ID), c.Kind, ui.MutedS.Render(c.URL)})
				}
				fmt.Println(ui.Table([]string{"ID", "CHANNEL", "WEBHOOK"}, rows))
			}
			fmt.Println()
			fmt.Println(ui.KV(
				"Disk", a.Thresholds["disk"]+"% used",
				"Memory", a.Thresholds["memory"]+"% used",
				"Temperature", a.Thresholds["temperature"]+"°C",
				"Crash loop", a.Thresholds["restarts"]+" restarts / 10 min",
				"Always on", "site down · deploy failed · certificate errors · out of memory · throttling",
			))
			fmt.Println()
			if len(a.Firing) == 0 {
				ui.Success("Nothing firing")
			} else {
				for _, f := range a.Firing {
					fmt.Println(ui.FailIcon + " " + f)
				}
			}
			return nil
		},
	}
}

func alertsAddCmd() *cobra.Command {
	return &cobra.Command{
		Use: "alerts:add <discord|slack> <webhook-url>", Short: "Send alerts to a Discord or Slack webhook",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var out map[string]int
			if err := s.c.Post(cmd.Context(), "/v1/alerts/channels", api.AlertChannel{Kind: args[0], URL: args[1]}, &out); err != nil {
				return err
			}
			ui.Success("Added %s channel #%d", args[0], out["id"])
			ui.Hint("Try it: %s", ui.Code("dokwalt alerts:test"))
			return nil
		},
	}
}

func alertsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use: "alerts:remove <id>", Short: "Remove an alert channel",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.c.Delete(cmd.Context(), "/v1/alerts/channels/"+url.PathEscape(args[0]), nil); err != nil {
				return err
			}
			ui.Success("Removed channel #%s", args[0])
			return nil
		},
	}
}

func alertsTestCmd() *cobra.Command {
	return &cobra.Command{
		Use: "alerts:test", Short: "Send a test alert to every channel",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var out struct {
				Errors []string `json:"errors"`
			}
			if err := s.c.Post(cmd.Context(), "/v1/alerts/test", nil, &out); err != nil {
				return err
			}
			if len(out.Errors) > 0 {
				for _, e := range out.Errors {
					ui.Fail("%s", e)
				}
				return errSilent
			}
			ui.Success("Test alert sent — check your channel(s)")
			return nil
		},
	}
}

func alertsSetCmd() *cobra.Command {
	return &cobra.Command{
		Use: "alerts:set key=value...", Short: "Set alert thresholds (disk, memory, temperature, restarts; 0 disables)",
		Example: "  dokwalt alerts:set disk=85 temperature=75",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set, err := parseAssignments(args)
			if err != nil {
				return err
			}
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.c.Post(cmd.Context(), "/v1/alerts/settings", set, nil); err != nil {
				return err
			}
			ui.Success("Thresholds updated")
			return nil
		},
	}
}

// ---- doctor ----

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use: "doctor", Short: "Diagnose the server, the apps and this machine",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var checks []api.DoctorCheck
			add := func(name, status, msg, hint string) {
				checks = append(checks, api.DoctorCheck{Name: name, Status: status, Message: msg, Hint: hint})
			}
			// This machine.
			if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output(); err == nil {
				add("Local Docker", "ok", "Docker "+strings.TrimSpace(string(out))+" (builds run here)", "")
			} else {
				add("Local Docker", "warn", "not available — needed to build images for deploy", "install Docker Desktop or OrbStack")
			}
			var s *session
			err := ui.Spin("Connecting", func() error {
				var err error
				s, err = connect(cmd.Context())
				return err
			})
			if err != nil {
				add("Connection", "fail", err.Error(), "check `ssh <server>` works")
				printChecks(checks)
				return errSilent
			}
			defer s.Close()
			add("Connection", "ok", fmt.Sprintf("%s (%s), daemon %s on linux/%s", s.name, s.server.Target, s.remote.Build, s.remote.Arch), "")
			if s.remote.Build != Build {
				add("Versions", "warn", fmt.Sprintf("CLI %s, server %s", Build, s.remote.Build), "dokwalt server upgrade")
			}
			var remote []api.DoctorCheck
			if err := ui.Spin("Running server checks", func() error { return s.c.Get(cmd.Context(), "/v1/doctor", &remote) }); err != nil {
				return err
			}
			checks = append(checks, remote...)
			if gf.json {
				return printJSON(checks)
			}
			printChecks(checks)
			for _, c := range checks {
				if c.Status == "fail" {
					return errSilent
				}
			}
			return nil
		},
	}
}

func printChecks(checks []api.DoctorCheck) {
	fmt.Println(ui.TitleS.Render("◆ Doctor"))
	fmt.Println()
	w := 0
	for _, c := range checks {
		if len(c.Name) > w {
			w = len(c.Name)
		}
	}
	counts := map[string]int{}
	for _, c := range checks {
		counts[c.Status]++
		icon := ui.OkIcon
		msg := ui.MutedS.Render(c.Message)
		switch c.Status {
		case "warn":
			icon, msg = ui.WarnIcon, ui.YellowS.Render(c.Message)
		case "fail":
			icon, msg = ui.FailIcon, ui.RedS.Render(c.Message)
		}
		fmt.Printf("%s %-*s  %s\n", icon, w, c.Name, msg)
		if c.Hint != "" && c.Status != "ok" {
			fmt.Printf("  %s  %s\n", strings.Repeat(" ", w), ui.FaintS.Render("→ ")+ui.Code(c.Hint))
		}
	}
	fmt.Println()
	summary := fmt.Sprintf("%d ok", counts["ok"])
	if counts["warn"] > 0 {
		summary += ui.YellowS.Render(fmt.Sprintf(" · %d warning(s)", counts["warn"]))
	}
	if counts["fail"] > 0 {
		summary += ui.RedS.Render(fmt.Sprintf(" · %d problem(s)", counts["fail"]))
	}
	fmt.Println(ui.MutedS.Render("  ") + summary)
}
