// Package cli implements the `dokwalt` command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/client"
	"github.com/ddahan/dokwalt/internal/sshx"
	"github.com/ddahan/dokwalt/internal/ui"
)

// Build is set at link time: -ldflags "-X github.com/ddahan/dokwalt/internal/cli.Build=v0.1.0"
var Build = "dev"

type globalFlags struct {
	app    string
	stage  string
	server string
	json   bool
}

var gf globalFlags

// errSilent means the error was already printed.
var errSilent = errors.New("")

func Execute() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	root := newRoot()
	if err := root.ExecuteContext(ctx); err != nil {
		if err != errSilent {
			printError(err)
		}
		return 1
	}
	return 0
}

func printError(err error) {
	if errors.Is(err, context.Canceled) {
		ui.Warn("Interrupted — a deploy or other operation already started keeps running on the server (check `dokwalt releases`).")
		return
	}
	var ae *client.APIError
	if errors.As(err, &ae) {
		ui.Fail("%s", ae.Error())
		if ae.Hint() != "" {
			ui.Hint("%s", ae.Hint())
		}
		return
	}
	msg := err.Error()
	first, rest, _ := strings.Cut(msg, "\n")
	ui.Fail("%s", first)
	if rest != "" {
		for _, l := range strings.Split(rest, "\n") {
			fmt.Fprintln(os.Stderr, ui.FaintS.Render("  │ ")+ui.MutedS.Render(l))
		}
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "dokwalt",
		Short:         "Deploy Docker Compose apps to your own server — from a gorgeous CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !ui.IsTTY() && os.Getenv("DOKWALT_SNAPSHOT") == "" {
				return cmd.Help()
			}
			return runDashboard(cmd.Context())
		},
	}
	root.PersistentFlags().StringVarP(&gf.app, "app", "a", "", "app name (default: the app linked to this folder)")
	root.PersistentFlags().StringVarP(&gf.stage, "stage", "s", api.DefaultStage, "pipeline stage (only for apps with a pipeline)")
	root.PersistentFlags().StringVar(&gf.server, "server", "", "server context (default: current)")
	root.PersistentFlags().BoolVar(&gf.json, "json", false, "machine-readable JSON output")

	groups := []*cobra.Group{
		{ID: "apps", Title: "Apps:"},
		{ID: "deploy", Title: "Deploy & releases:"},
		{ID: "config", Title: "Configuration:"},
		{ID: "observe", Title: "Logs & monitoring:"},
		{ID: "server", Title: "Server:"},
	}
	root.AddGroup(groups...)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("apps", appsCommands()...)
	add("deploy", deployCommands()...)
	add("config", configCommands()...)
	add("observe", observeCommands()...)
	add("server", serverCommands()...)
	root.AddCommand(hiddenCommands()...)
	root.SetHelpTemplate(helpTemplate)
	root.SetUsageTemplate(usageTemplate)
	cobra.AddTemplateFunc("title", func(s string) string { return ui.TitleS.Render(s) })
	cobra.AddTemplateFunc("muted", func(s string) string { return ui.MutedS.Render(s) })
	cobra.AddTemplateFunc("cmd", func(s string) string { return ui.AccentS.Render(s) })
	return root
}

const helpTemplate = `{{with (or .Long .Short)}}{{title "◆ DokWalt"}}  {{.}}{{end}}

{{if or .Runnable .HasSubCommands}}{{.UsageString}}{{end}}`

const usageTemplate = `{{muted "Usage:"}}{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} <command> [flags]{{end}}{{if gt (len .Aliases) 0}}

{{muted "Aliases:"}}
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

{{muted "Examples:"}}
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

{{muted "Commands:"}}{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{cmd (rpad .Name .NamePadding)}} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{muted .Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{cmd (rpad .Name .NamePadding)}} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

{{muted "Flags:"}}
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

{{muted "Global flags:"}}
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableSubCommands}}

Run "{{.CommandPath}} docs" for guides, "{{.CommandPath}} <command> --help" for details.{{end}}
`

// ---- connection helpers ----

type session struct {
	cfg    *LocalConfig
	name   string // server context name
	server ServerConfig
	c      *client.Client
	ssh    *sshx.Client
	remote api.VersionInfo
}

func (s *session) Close() {
	if s.ssh != nil {
		s.ssh.Close()
	}
}

func prompt(q string) bool { return ui.Confirm(q) }

// serverName picks the server context: flag, folder link, current.
func serverName(cfg *LocalConfig) (string, error) {
	if gf.server != "" {
		if _, ok := cfg.Servers[gf.server]; !ok {
			return "", fmt.Errorf("unknown server %q — see `dokwalt server list`", gf.server)
		}
		return gf.server, nil
	}
	wd, _ := os.Getwd()
	if l, _, ok := cfg.LinkFor(wd); ok && l.Server != "" {
		if _, ok := cfg.Servers[l.Server]; ok {
			return l.Server, nil
		}
	}
	if cfg.Current != "" {
		return cfg.Current, nil
	}
	if len(cfg.Servers) == 1 {
		for n := range cfg.Servers {
			return n, nil
		}
	}
	return "", errors.New("no server configured — run `dokwalt server init user@your-server` first")
}

func connect(ctx context.Context) (*session, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	name, err := serverName(cfg)
	if err != nil {
		return nil, err
	}
	return connectTo(ctx, cfg, name)
}

func connectTo(ctx context.Context, cfg *LocalConfig, name string) (*session, error) {
	sc := cfg.Servers[name]
	s := &session{cfg: cfg, name: name, server: sc}
	if path, ok := strings.CutPrefix(sc.Target, "unix:"); ok {
		s.c = client.Local(path)
	} else {
		var err error
		s.ssh, err = sshx.Dial(sc.Target, prompt)
		if err != nil {
			return nil, fmt.Errorf("connect to %s (%s): %w", name, sc.Target, err)
		}
		s.c = client.Remote(s.ssh, sc.Bin)
	}
	if err := s.c.Get(ctx, "/v1/version", &s.remote); err != nil {
		s.Close()
		return nil, err
	}
	if major(s.remote.APIVersion) != major(api.Version) {
		s.Close()
		return nil, fmt.Errorf("server speaks API %s but this CLI speaks %s — run `dokwalt server upgrade` (or update the CLI)", s.remote.APIVersion, api.Version)
	}
	return s, nil
}

func major(v string) string {
	m, _, _ := strings.Cut(v, ".")
	return m
}

// appName resolves the target app.
func appName(cfg *LocalConfig) (string, error) {
	if gf.app != "" {
		return gf.app, nil
	}
	wd, _ := os.Getwd()
	if l, _, ok := cfg.LinkFor(wd); ok {
		return l.App, nil
	}
	return "", errors.New("which app? pass -a <app>, or run `dokwalt link <app>` in your project folder")
}

// appStage connects and resolves app + stage.
func appStage(ctx context.Context) (*session, string, string, error) {
	s, err := connect(ctx)
	if err != nil {
		return nil, "", "", err
	}
	app, err := appName(s.cfg)
	if err != nil {
		s.Close()
		return nil, "", "", err
	}
	return s, app, gf.stage, nil
}

// label is "app" or "app (staging)".
func label(app, stage string) string {
	if stage == api.DefaultStage {
		return app
	}
	return app + " (" + stage + ")"
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// runOp runs a streamed operation with live progress.
func runOp(ctx context.Context, s *session, method, path string, in any) (api.Event, error) {
	return ui.Operation(func(emit func(api.Event)) (api.Event, error) {
		return s.c.Operation(ctx, method, path, in, emit)
	})
}
