package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dokwalt/dokwalt/internal/api"
	"github.com/dokwalt/dokwalt/internal/client"
	"github.com/dokwalt/dokwalt/internal/ui"
)

func appsCommands() []*cobra.Command {
	return []*cobra.Command{
		appsListCmd(), appsCreateCmd(), appsInfoCmd(), appsDestroyCmd(), appsExportCmd(), appsImportCmd(),
		linkCmd(), unlinkCmd(), psCmd(), restartCmd(), stopCmd(), startCmd(),
	}
}

func appsListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "apps", Short: "List apps and their status",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var apps []api.App
			if err := s.c.Get(cmd.Context(), "/v1/apps", &apps); err != nil {
				return err
			}
			if gf.json {
				return printJSON(apps)
			}
			if len(apps) == 0 {
				ui.Info("No apps on %s yet.", ui.Bold.Render(s.name))
				ui.Hint("Create one: %s", ui.Code("dokwalt apps:create myapp"))
				return nil
			}
			var rows [][]string
			for _, a := range apps {
				for i, st := range a.Stages {
					name := ui.Bold.Render(a.Name)
					if i > 0 {
						name = ""
					}
					stage := ""
					if a.Pipeline {
						stage = ui.MutedS.Render(st.Name)
					}
					rel := ui.MutedS.Render("—")
					if st.CurrentRelease > 0 {
						rel = fmt.Sprintf("v%d", st.CurrentRelease)
					}
					rows = append(rows, []string{name, stage, ui.Status(st.Status), rel, strings.Join(st.Domains, ", ")})
				}
			}
			fmt.Println(ui.Table([]string{"APP", "STAGE", "STATUS", "RELEASE", "DOMAINS"}, rows))
			return nil
		},
	}
}

func appsCreateCmd() *cobra.Command {
	var noLink bool
	cmd := &cobra.Command{
		Use: "apps:create <name>", Short: "Create an app (and link it to this folder)",
		Args:    cobra.ExactArgs(1),
		Example: "  dokwalt apps:create blog",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var a api.App
			if err := s.c.Post(cmd.Context(), "/v1/apps", map[string]string{"name": args[0]}, &a); err != nil {
				return err
			}
			ui.Success("Created %s on %s", ui.Bold.Render(a.Name), s.name)
			if !noLink {
				if err := saveLink(s, a.Name); err == nil {
					wd, _ := os.Getwd()
					ui.Hint("Linked to %s", wd)
				}
			}
			ui.Hint("Next: %s then %s", ui.Code("dokwalt deploy"), ui.Code("dokwalt domains:add example.com --service web"))
			return nil
		},
	}
	cmd.Flags().BoolVar(&noLink, "no-link", false, "don't link the current folder")
	return cmd
}

func saveLink(s *session, app string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	s.cfg.Links[wd] = Link{Server: s.name, App: app}
	return s.cfg.Save()
}

func appsInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use: "apps:info", Short: "Show an app: stages, services, domains, releases",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, _, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			ctx := cmd.Context()
			var a api.App
			if err := s.c.Get(ctx, "/v1/apps/"+url.PathEscape(app), &a); err != nil {
				return err
			}
			if gf.json {
				return printJSON(a)
			}
			fmt.Println(ui.TitleS.Render("◆ "+a.Name) + ui.MutedS.Render("  on "+s.name))
			for _, st := range a.Stages {
				fmt.Println()
				heading := "Status"
				if a.Pipeline {
					heading = strings.ToUpper(st.Name[:1]) + st.Name[1:]
				}
				rel := "—"
				if st.CurrentRelease > 0 {
					rel = fmt.Sprintf("v%d (%s)", st.CurrentRelease, st.ActiveColor)
					if st.ActiveColor == "" {
						rel = fmt.Sprintf("v%d", st.CurrentRelease)
					}
				}
				doms := strings.Join(st.Domains, ", ")
				if doms == "" {
					doms = ui.MutedS.Render("none — dokwalt domains:add <host>")
				}
				fmt.Println(ui.KV(heading, ui.Status(st.Status), "Release", rel, "Domains", doms))
				var svcs []api.Service
				if err := s.c.Get(ctx, client.StagePath(app, st.Name, "/services"), &svcs); err == nil && len(svcs) > 0 {
					fmt.Println()
					printServices(svcs)
				}
			}
			return nil
		},
	}
}

func printServices(svcs []api.Service) {
	var rows [][]string
	for _, sv := range svcs {
		kind := "stateless"
		if sv.Stateful {
			kind = ui.AccentS.Render("stateful")
		}
		state := ui.MutedS.Render("—")
		if len(sv.Containers) > 0 {
			running := 0
			for _, c := range sv.Containers {
				if c.State == "running" {
					running++
				}
			}
			st := "running"
			if running < len(sv.Containers) {
				st = "degraded"
			}
			if running == 0 {
				st = sv.Containers[0].State
			}
			state = ui.Status(st) + ui.MutedS.Render(fmt.Sprintf(" %d/%d", running, len(sv.Containers)))
			for _, c := range sv.Containers {
				if c.Health != "" && c.Health != "healthy" {
					state += " " + ui.YellowS.Render(c.Health)
				}
			}
		}
		cpu, mem := ui.MutedS.Render("—"), ui.MutedS.Render("—")
		if sv.Metrics != nil {
			cpu = fmt.Sprintf("%.1f%%", sv.Metrics.CPUPct)
			mem = ui.Bytes(sv.Metrics.MemBytes)
		}
		rows = append(rows, []string{ui.ServiceColor(sv.Name).Render(sv.Name), state, kind, cpu, mem, ui.MutedS.Render(sv.Image)})
	}
	fmt.Println(ui.Table([]string{"SERVICE", "STATE", "KIND", "CPU", "MEM", "IMAGE"}, rows))
}

func appsDestroyCmd() *cobra.Command {
	var confirm string
	cmd := &cobra.Command{
		Use: "apps:destroy <name>", Short: "Delete an app with all its containers, volumes and data",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			name := args[0]
			if confirm != name && !ui.ConfirmName(fmt.Sprintf("This permanently deletes %s on %s, including its volumes (databases!).", name, s.name), name) {
				return errors.New("aborted")
			}
			if _, err := runOp(cmd.Context(), s, "DELETE", "/v1/apps/"+url.PathEscape(name), nil); err != nil {
				return err
			}
			for dir, l := range s.cfg.Links {
				if l.App == name && l.Server == s.name {
					delete(s.cfg.Links, dir)
				}
			}
			_ = s.cfg.Save()
			ui.Success("Destroyed %s", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&confirm, "confirm", "", "app name, to skip the interactive confirmation")
	return cmd
}

func appsExportCmd() *cobra.Command {
	return &cobra.Command{
		Use: "apps:export [file]", Short: "Export an app's server-side settings (config, domains, overrides)",
		Long: "Export an app's server-side settings as JSON — config vars (in clear text!), domains and service overrides — to recreate it on another server with apps:import. Images and volume data are not included.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, _, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var exp api.AppExport
			if err := s.c.Get(cmd.Context(), "/v1/apps/"+url.PathEscape(app)+"/export", &exp); err != nil {
				return err
			}
			b, _ := json.MarshalIndent(exp, "", "  ")
			if len(args) == 0 {
				fmt.Println(string(b))
				return nil
			}
			if err := os.WriteFile(args[0], b, 0o600); err != nil {
				return err
			}
			ui.Success("Exported %s to %s", app, args[0])
			ui.Warn("The file contains secrets in clear text: keep it safe.")
			return nil
		},
	}
}

func appsImportCmd() *cobra.Command {
	return &cobra.Command{
		Use: "apps:import <file>", Short: "Create an app from an apps:export file",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var exp api.AppExport
			if err := json.Unmarshal(b, &exp); err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			if gf.app != "" {
				exp.Name = gf.app
			}
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.c.Post(cmd.Context(), "/v1/apps/import", exp, nil); err != nil {
				return err
			}
			ui.Success("Imported %s on %s", exp.Name, s.name)
			ui.Hint("Deploy it from its project folder: %s", ui.Code("dokwalt link "+exp.Name+" && dokwalt deploy"))
			return nil
		},
	}
}

func linkCmd() *cobra.Command {
	return &cobra.Command{
		Use: "link <app>", Short: "Link this folder to an app (so -a can be omitted)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var a api.App
			if err := s.c.Get(cmd.Context(), "/v1/apps/"+url.PathEscape(args[0]), &a); err != nil {
				if client.IsNotFound(err) {
					return fmt.Errorf("app %q doesn't exist on %s — create it with `dokwalt apps:create %s`", args[0], s.name, args[0])
				}
				return err
			}
			if err := saveLink(s, a.Name); err != nil {
				return err
			}
			wd, _ := os.Getwd()
			ui.Success("Linked %s to %s on %s", filepath.Base(wd), ui.Bold.Render(a.Name), s.name)
			return nil
		},
	}
}

func unlinkCmd() *cobra.Command {
	return &cobra.Command{
		Use: "unlink", Short: "Remove this folder's app link",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			wd, _ := os.Getwd()
			l, dir, ok := cfg.LinkFor(wd)
			if !ok {
				return errors.New("this folder is not linked")
			}
			delete(cfg.Links, dir)
			if err := cfg.Save(); err != nil {
				return err
			}
			ui.Success("Unlinked %s from %s", dir, l.App)
			return nil
		},
	}
}

func psCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ps", Short: "Show the app's services and containers",
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
				ui.Info("%s is not deployed yet — run %s", label(app, stage), ui.Code("dokwalt deploy"))
				return nil
			}
			var rows [][]string
			sort.Slice(svcs, func(i, j int) bool { return svcs[i].Name < svcs[j].Name })
			for _, sv := range svcs {
				for _, c := range sv.Containers {
					if c.State == "exited" && strings.HasPrefix(c.Status, "Exited (0)") {
						rows = append(rows, []string{ui.ServiceColor(sv.Name).Render(sv.Name), c.Name, ui.GreenS.Render("✓ completed"), ui.MutedS.Render("—"), "0", ui.MutedS.Render(c.Status)})
						continue
					}
					health := c.Health
					if health == "" {
						health = ui.MutedS.Render("—")
					}
					restarts := fmt.Sprint(c.Restarts)
					if c.Restarts > 0 {
						restarts = ui.YellowS.Render(restarts)
					}
					rows = append(rows, []string{ui.ServiceColor(sv.Name).Render(sv.Name), c.Name, ui.Status(c.State), health, restarts, ui.MutedS.Render(c.Status)})
				}
				if len(sv.Containers) == 0 {
					rows = append(rows, []string{ui.ServiceColor(sv.Name).Render(sv.Name), ui.MutedS.Render("—"), ui.Status("down"), "", "", ""})
				}
			}
			fmt.Println(ui.Table([]string{"SERVICE", "CONTAINER", "STATE", "HEALTH", "RESTARTS", "STATUS"}, rows))
			return nil
		},
	}
}

func restartCmd() *cobra.Command {
	var service string
	cmd := &cobra.Command{
		Use: "restart", Short: "Restart the app's containers (one at a time)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var out map[string]int
			err = ui.Spin("Restarting "+label(app, stage), func() error {
				return s.c.Post(cmd.Context(), client.StagePath(app, stage, "/restart?service="+url.QueryEscape(service)), nil, &out)
			})
			if err != nil {
				return err
			}
			ui.Success("Restarted %d container(s)", out["restarted"])
			return nil
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "only this service")
	return cmd
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use: "stop", Short: "Stop the app (stays stopped across reboots)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if err := ui.Spin("Stopping "+label(app, stage), func() error {
				return s.c.Post(cmd.Context(), client.StagePath(app, stage, "/stop"), nil, nil)
			}); err != nil {
				return err
			}
			ui.Success("%s stopped — its domains show a maintenance page. %s to bring it back.", label(app, stage), ui.Code("dokwalt start"))
			return nil
		},
	}
}

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use: "start", Short: "Start a stopped app",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, app, stage, err := appStage(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if err := ui.Spin("Starting "+label(app, stage), func() error {
				return s.c.Post(cmd.Context(), client.StagePath(app, stage, "/start"), nil, nil)
			}); err != nil {
				return err
			}
			ui.Success("%s started", label(app, stage))
			return nil
		},
	}
}
