package cli

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dokwalt/dokwalt/internal/api"
	"github.com/dokwalt/dokwalt/internal/client"
	"github.com/dokwalt/dokwalt/internal/ui"
)

func configCommands() []*cobra.Command {
	return []*cobra.Command{
		configListCmd(), configGetCmd(), configSetCmd(), configUnsetCmd(), configImportCmd(),
		domainsListCmd(), domainsAddCmd(), domainsRemoveCmd(), domainsRedirectCmd(),
		healthcheckSetCmd(), healthcheckUnsetCmd(), servicesCmd(), servicesSetCmd(),
	}
}

func configListCmd() *cobra.Command {
	var reveal, shell bool
	cmd := &cobra.Command{
		Use: "config", Short: "List config vars (values masked unless --reveal)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			q := ""
			if reveal || shell {
				q = "?reveal=1"
			}
			var vars []api.ConfigVar
			if err := s.c.Get(cmd.Context(), client.StagePath(app, stage, "/config"+q), &vars); err != nil {
				return err
			}
			if gf.json {
				m := map[string]string{}
				for _, v := range vars {
					m[v.Key] = v.Value
				}
				return printJSON(m)
			}
			if shell {
				for _, v := range vars {
					fmt.Printf("%s=%s\n", v.Key, strconv.Quote(v.Value))
				}
				return nil
			}
			if len(vars) == 0 {
				ui.Info("No config vars for %s", label(app, stage))
				ui.Hint("Set one: %s", ui.Code("dokwalt config:set KEY=value"))
				return nil
			}
			fmt.Println(ui.TitleS.Render("◆ Config of " + label(app, stage)))
			var rows [][]string
			for _, v := range vars {
				rows = append(rows, []string{ui.AccentS.Render(v.Key), v.Value})
			}
			fmt.Println(ui.Table([]string{"KEY", "VALUE"}, rows))
			return nil
		},
	}
	cmd.Flags().BoolVar(&reveal, "reveal", false, "show values in clear text")
	cmd.Flags().BoolVar(&shell, "shell", false, "print KEY=value lines (clear text)")
	return cmd
}

func configGetCmd() *cobra.Command {
	return &cobra.Command{
		Use: "config:get <KEY>", Short: "Print one config value",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var vars []api.ConfigVar
			if err := s.c.Get(cmd.Context(), client.StagePath(app, stage, "/config?reveal=1"), &vars); err != nil {
				return err
			}
			for _, v := range vars {
				if v.Key == args[0] {
					fmt.Println(v.Value)
					return nil
				}
			}
			return fmt.Errorf("%s is not set", args[0])
		},
	}
}

func parseAssignments(args []string) (map[string]string, error) {
	set := map[string]string{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid %q — use KEY=value", a)
		}
		set[k] = v
	}
	return set, nil
}

func applyConfig(cmd *cobra.Command, req api.ConfigSetRequest) error {
	s, app, stage, err := appStage(cmd.Context())
	if err != nil {
		return err
	}
	defer s.Close()
	ev, err := runOp(cmd.Context(), s, "POST", client.StagePath(app, stage, "/config"), req)
	if err != nil {
		return err
	}
	if ev.Release > 0 {
		ui.Success("Config updated — %s restarted as v%d with zero downtime", label(app, stage), ev.Release)
	} else if req.NoRestart {
		ui.Success("Config updated — applies on next deploy")
	} else {
		ui.Success("Config updated — applies on first deploy")
	}
	return nil
}

func configSetCmd() *cobra.Command {
	var noRestart bool
	cmd := &cobra.Command{
		Use: "config:set KEY=value...", Short: "Set config vars (creates a release)",
		Example: "  dokwalt config:set SECRET_KEY=s3cr3t DEBUG=false\n  dokwalt config:set -s staging API_URL=https://staging-api.example.com",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set, err := parseAssignments(args)
			if err != nil {
				return err
			}
			return applyConfig(cmd, api.ConfigSetRequest{Set: set, NoRestart: noRestart})
		},
	}
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "store without restarting (applies on next deploy)")
	return cmd
}

func configUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use: "config:unset KEY...", Short: "Remove config vars (creates a release)",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return applyConfig(cmd, api.ConfigSetRequest{Unset: args})
		},
	}
}

func configImportCmd() *cobra.Command {
	return &cobra.Command{
		Use: "config:import <.env file>", Short: "Import config vars from a .env file",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set, err := parseDotenv(args[0])
			if err != nil {
				return err
			}
			if len(set) == 0 {
				return errors.New("no variables found")
			}
			ui.Info("Importing %d variable(s) from %s", len(set), args[0])
			return applyConfig(cmd, api.ConfigSetRequest{Set: set})
		},
	}
}

// parseDotenv reads KEY=value lines (export prefix, quotes and comments supported).
func parseDotenv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=value", path, n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			if uq, err := strconv.Unquote(v); err == nil {
				v = uq
			} else {
				v = v[1 : len(v)-1]
			}
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
		}
		out[k] = v
	}
	return out, sc.Err()
}

// ---- domains ----

func domainsListCmd() *cobra.Command {
	var noCheck bool
	cmd := &cobra.Command{
		Use: "domains", Short: "List domains and check they reach this server",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			q := "?check=1"
			if noCheck {
				q = ""
			}
			var doms []api.Domain
			load := func() error { return s.c.Get(cmd.Context(), client.StagePath(app, stage, "/domains"+q), &doms) }
			if gf.json || noCheck {
				err = load()
			} else {
				err = ui.Spin("Checking domains", load)
			}
			if err != nil {
				return err
			}
			if gf.json {
				return printJSON(doms)
			}
			if len(doms) == 0 {
				ui.Info("No domains for %s", label(app, stage))
				ui.Hint("Add one: %s", ui.Code("dokwalt domains:add example.com --service web"))
				return nil
			}
			var rows [][]string
			for _, d := range doms {
				port := "auto"
				if d.Port > 0 {
					port = fmt.Sprint(d.Port)
				}
				target := ui.ServiceColor(d.Service).Render(d.Service) + ":" + port
				if d.RedirectTo != "" {
					target = ui.ArrowIcon + " " + d.RedirectTo
				}
				check := ui.MutedS.Render("—")
				if d.Check != "" {
					if strings.HasPrefix(d.Check, "ok") {
						check = ui.OkIcon + " " + ui.MutedS.Render(d.Check)
					} else {
						check = ui.WarnIcon + " " + ui.YellowS.Render(d.Check)
					}
				}
				rows = append(rows, []string{ui.Bold.Render(d.Hostname), target, check})
			}
			fmt.Println(ui.Table([]string{"DOMAIN", "TARGET", "REACHABILITY"}, rows))
			return nil
		},
	}
	cmd.Flags().BoolVar(&noCheck, "no-check", false, "don't probe DNS/reachability")
	return cmd
}

func printDomainResult(d struct {
	api.Domain
	Warnings []string `json:"warnings"`
}) {
	for _, w := range d.Warnings {
		ui.Warn("%s", w)
	}
	switch {
	case strings.HasPrefix(d.Check, "ok"):
		ui.Success("%s → reachable; HTTPS certificate is issued automatically", ui.Bold.Render(d.Hostname))
	default:
		ui.Warn("%s was added but isn't reachable yet:", d.Hostname)
		ui.Hint("%s", d.Check)
		ui.Hint("Caddy keeps retrying the certificate; check again with %s", ui.Code("dokwalt domains"))
	}
}

func domainsAddCmd() *cobra.Command {
	var service string
	var port int
	cmd := &cobra.Command{
		Use: "domains:add <host>", Short: "Route a domain to a service (automatic HTTPS)",
		Example: "  dokwalt domains:add example.com --service web\n  dokwalt domains:add api.example.com --service api --port 8080",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var out struct {
				api.Domain
				Warnings []string `json:"warnings"`
			}
			err = ui.Spin("Adding "+args[0], func() error {
				return s.c.Post(cmd.Context(), client.StagePath(app, stage, "/domains"), api.DomainAddRequest{Hostname: args[0], Service: service, Port: port}, &out)
			})
			if err != nil {
				return err
			}
			port := "port from compose file"
			if out.Port > 0 {
				port = fmt.Sprint(out.Port)
			}
			fmt.Println(ui.MutedS.Render(fmt.Sprintf("  %s → %s:%s", out.Hostname, out.Service, port)))
			printDomainResult(out)
			return nil
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "compose service to route to")
	cmd.Flags().IntVar(&port, "port", 0, "container port (default: the service's declared port)")
	return cmd
}

func domainsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use: "domains:remove <host>", Short: "Remove a domain",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.c.Delete(cmd.Context(), client.StagePath(app, stage, "/domains/"+url.PathEscape(args[0])), nil); err != nil {
				return err
			}
			ui.Success("Removed %s", args[0])
			return nil
		},
	}
}

func domainsRedirectCmd() *cobra.Command {
	return &cobra.Command{
		Use: "domains:redirect <from> <to>", Short: "Redirect a domain to another (308, keeps the path)",
		Example: "  dokwalt domains:redirect www.example.com example.com",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var out struct {
				api.Domain
				Warnings []string `json:"warnings"`
			}
			if err := s.c.Post(cmd.Context(), client.StagePath(app, stage, "/domains"), api.DomainAddRequest{Hostname: args[0], RedirectTo: args[1]}, &out); err != nil {
				return err
			}
			fmt.Println(ui.MutedS.Render(fmt.Sprintf("  https://%s → https://%s", args[0], args[1])))
			printDomainResult(out)
			return nil
		},
	}
}

// ---- health checks & services ----

func healthcheckSetCmd() *cobra.Command {
	var service, path string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use: "healthcheck:set", Short: "Set the HTTP path checked before a new version gets traffic",
		Example: "  dokwalt healthcheck:set --service web --path /health --timeout 90s",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if service == "" || path == "" {
				return errors.New("--service and --path are required")
			}
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			in := api.ServiceOverride{HealthPath: path, HealthTimeout: int(timeout.Seconds())}
			if err := s.c.Post(cmd.Context(), client.StagePath(app, stage, "/services/"+url.PathEscape(service)), in, nil); err != nil {
				return err
			}
			ui.Success("Deploys of %s now wait for GET %s to answer 2xx/3xx (timeout %s)", service, path, timeout)
			return nil
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "service")
	cmd.Flags().StringVar(&path, "path", "", "HTTP path, e.g. /health")
	cmd.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "how long to wait for the check to pass")
	return cmd
}

func healthcheckUnsetCmd() *cobra.Command {
	var service string
	cmd := &cobra.Command{
		Use: "healthcheck:unset", Short: "Back to the default check (any HTTP answer below 500)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if service == "" {
				return errors.New("--service is required")
			}
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.c.Post(cmd.Context(), client.StagePath(app, stage, "/services/"+url.PathEscape(service)), api.ServiceOverride{ClearHealth: true}, nil); err != nil {
				return err
			}
			ui.Success("Health check of %s reset to default", service)
			return nil
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "service")
	return cmd
}

func servicesCmd() *cobra.Command {
	return &cobra.Command{
		Use: "services", Short: "Show how each service is handled (stateful or blue/green)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var svcs []api.Service
			if err := s.c.Get(cmd.Context(), client.StagePath(app, stage, "/services"), &svcs); err != nil {
				return err
			}
			if gf.json {
				return printJSON(svcs)
			}
			if len(svcs) == 0 {
				ui.Info("%s is not deployed yet", label(app, stage))
				return nil
			}
			var rows [][]string
			for _, sv := range svcs {
				mode := ui.GreenS.Render("blue/green") + ui.MutedS.Render(" (zero downtime)")
				if sv.Stateful {
					mode = ui.AccentS.Render("stateful") + ui.MutedS.Render(" (updated in place, never duplicated)")
				}
				why := sv.Detected
				if sv.Overridden {
					why = ui.YellowS.Render(why)
				}
				hc := sv.HealthPath
				if hc == "" {
					hc = ui.MutedS.Render("default")
				}
				rows = append(rows, []string{ui.ServiceColor(sv.Name).Render(sv.Name), mode, ui.MutedS.Render(why), hc})
			}
			fmt.Println(ui.Table([]string{"SERVICE", "DEPLOY MODE", "WHY", "HEALTH CHECK"}, rows))
			ui.Hint("Override: %s", ui.Code("dokwalt services:set <service> --stateful=false"))
			return nil
		},
	}
}

func servicesSetCmd() *cobra.Command {
	var stateful bool
	cmd := &cobra.Command{
		Use: "services:set <service> --stateful=true|false", Short: "Override stateful detection for a service",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("stateful") {
				return errors.New("pass --stateful=true or --stateful=false")
			}
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if _, err := runOp(cmd.Context(), s, "POST", client.StagePath(app, stage, "/services/"+url.PathEscape(args[0])), api.ServiceOverride{Stateful: &stateful}); err != nil {
				return err
			}
			ui.Success("%s is now treated as stateful=%v", args[0], stateful)
			return nil
		},
	}
	cmd.Flags().BoolVar(&stateful, "stateful", false, "treat as stateful (never duplicated)")
	return cmd
}
