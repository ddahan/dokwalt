package cli

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dokwalt/dokwalt/internal/api"
	"github.com/dokwalt/dokwalt/internal/bootstrap"
	"github.com/dokwalt/dokwalt/internal/sshx"
	"github.com/dokwalt/dokwalt/internal/ui"
)

const releaseURL = "https://github.com/dokwalt/dokwalt/releases/download"

func serverCommands() []*cobra.Command {
	server := &cobra.Command{Use: "server", Short: "Install, connect and manage servers"}
	server.AddCommand(serverInitCmd(), serverAddCmd(), serverListCmd(), serverUseCmd(), serverRemoveCmd(), serverInfoCmd(), serverSettingsCmd(), serverUpgradeCmd(), serverBootstrapCmd())
	return []*cobra.Command{server}
}

func serverInitCmd() *cobra.Command {
	var name, email, binary string
	cmd := &cobra.Command{
		Use:   "init <user@host>",
		Short: "Install DokWalt on a server over SSH (Docker, daemon, proxy)",
		Long: `Install DokWalt on a fresh Debian/Ubuntu/Raspberry Pi OS server:
Docker (if missing), the dokwalt daemon as a systemd service, and the Caddy
reverse proxy. You'll be asked for your sudo password on the server.`,
		Example: "  dokwalt server init pi@raspberrypi.local --email you@example.com\n  dokwalt server init deploy@203.0.113.10 --name prod --email you@example.com",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			target := args[0]
			if name == "" {
				name = defaultServerName(target)
			}
			if err := install(cmd, target, binary, email); err != nil {
				return err
			}
			cfg.Servers[name] = ServerConfig{Target: target}
			if cfg.Current == "" {
				cfg.Current = name
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			s, err := connectTo(cmd.Context(), cfg, name)
			if err != nil {
				return fmt.Errorf("installed, but connecting as your user failed: %w", err)
			}
			defer s.Close()
			fmt.Println()
			ui.Success("%s is ready — saved as server %s", ui.Bold.Render(target), ui.Bold.Render(name))
			fmt.Println()
			ui.Hint("Next, in your project folder:")
			ui.Hint("  %s", ui.Code("dokwalt apps:create myapp"))
			ui.Hint("  %s", ui.Code("dokwalt deploy"))
			ui.Hint("  %s", ui.Code("dokwalt domains:add myapp.example.com --service web"))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name for this server (default: derived from the host)")
	cmd.Flags().StringVar(&email, "email", "", "email for Let's Encrypt expiry notices")
	cmd.Flags().StringVar(&binary, "binary", "", "path to a linux dokwalt binary to install (development)")
	return cmd
}

func defaultServerName(target string) string {
	host := target
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	host, _, _ = strings.Cut(host, ":")
	if strings.Count(host, ".") == 3 && !strings.ContainsAny(host, "abcdefghijklmnopqrstuvwxyz") {
		return "default"
	}
	h, _, _ := strings.Cut(host, ".")
	if h == "" {
		return "default"
	}
	return h
}

// install uploads the right binary and runs the bootstrap with sudo in a PTY.
func install(cmd *cobra.Command, target, binary, email string) error {
	fmt.Println(ui.TitleS.Render("◆ Installing DokWalt on " + target))
	var c *sshx.Client
	if err := ui.Spin("Connecting over SSH", func() error {
		var err error
		c, err = sshx.Dial(target, nil)
		return err
	}); err != nil {
		// Maybe an unknown host key: retry interactively (outside the spinner).
		if strings.Contains(err.Error(), "not trusted") {
			c, err = sshx.Dial(target, prompt)
		}
		if err != nil {
			return err
		}
	}
	defer c.Close()
	uname, err := c.Output("uname -sm")
	if err != nil {
		return err
	}
	fields := strings.Fields(uname)
	if len(fields) != 2 || fields[0] != "Linux" {
		return fmt.Errorf("unsupported server %q: DokWalt needs Linux", strings.TrimSpace(uname))
	}
	arch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[fields[1]]
	if arch == "" {
		return fmt.Errorf("unsupported CPU %s (amd64 and arm64 are supported; 32-bit Raspberry Pi OS is not — use the 64-bit image)", fields[1])
	}
	bin, err := linuxBinary(arch, binary)
	if err != nil {
		return err
	}
	user, err := c.Output("id -un")
	if err != nil {
		return err
	}
	user = strings.TrimSpace(user)
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	remote := "/tmp/dokwalt-install-" + hex.EncodeToString(b)
	st, _ := os.Stat(bin)
	if err := ui.Spin(fmt.Sprintf("Uploading dokwalt for linux/%s (%s)", arch, ui.Bytes(uint64(st.Size()))), func() error {
		return c.Upload(bin, remote, 0o755, nil)
	}); err != nil {
		return err
	}
	sudo := "sudo "
	if user == "root" {
		sudo = ""
	}
	script := fmt.Sprintf("%s%s server bootstrap --user %s", sudo, remote, sshx.ShellQuote(user))
	if email != "" {
		script += " --email " + sshx.ShellQuote(email)
	}
	script += "; rc=$?; rm -f " + remote + "; exit $rc"
	if sudo != "" {
		ui.Info("Running the installer with sudo on the server (you may be asked for your password)")
	}
	if err := c.Interactive(script); err != nil {
		return fmt.Errorf("installation failed: %w", err)
	}
	return nil
}

// linuxBinary finds a dokwalt binary for linux/<arch>.
func linuxBinary(arch, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	exe, _ := os.Executable()
	if runtime.GOOS == "linux" && runtime.GOARCH == arch {
		return exe, nil
	}
	name := "dokwalt_linux_" + arch
	for _, p := range []string{filepath.Join(filepath.Dir(exe), name), filepath.Join("dist", name)} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if Build == "dev" {
		return "", fmt.Errorf("development build: build the server binary first (`make dist`) or pass --binary")
	}
	return download(arch)
}

// download fetches the release binary and verifies its checksum.
func download(arch string) (string, error) {
	cache, _ := os.UserCacheDir()
	dir := filepath.Join(cache, "dokwalt", Build)
	path := filepath.Join(dir, "dokwalt_linux_"+arch)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	get := func(url string) ([]byte, error) {
		c := &http.Client{Timeout: 5 * time.Minute}
		resp, err := c.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
	var bin []byte
	err := ui.Spin("Downloading dokwalt "+Build+" for linux/"+arch, func() error {
		sums, err := get(fmt.Sprintf("%s/%s/checksums.txt", releaseURL, Build))
		if err != nil {
			return err
		}
		bin, err = get(fmt.Sprintf("%s/%s/dokwalt_linux_%s", releaseURL, Build, arch))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(bin)
		want := ""
		sc := bufio.NewScanner(strings.NewReader(string(sums)))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 2 && f[1] == "dokwalt_linux_"+arch {
				want = f[0]
			}
		}
		if want != hex.EncodeToString(sum[:]) {
			return errors.New("checksum mismatch — download corrupted or tampered with")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, bin, 0o755)
}

func serverAddCmd() *cobra.Command {
	return &cobra.Command{
		Use: "add <name> <user@host>", Short: "Add an existing DokWalt server",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cfg.Servers[args[0]] = ServerConfig{Target: args[1]}
			s, err := connectTo(cmd.Context(), cfg, args[0])
			if err != nil {
				return err
			}
			s.Close()
			if cfg.Current == "" {
				cfg.Current = args[0]
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			ui.Success("Added %s (%s, daemon %s)", args[0], args[1], s.remote.Build)
			return nil
		},
	}
}

func serverListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List configured servers",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if gf.json {
				return printJSON(cfg.Servers)
			}
			if len(cfg.Servers) == 0 {
				ui.Info("No servers yet — %s", ui.Code("dokwalt server init user@host"))
				return nil
			}
			names := make([]string, 0, len(cfg.Servers))
			for n := range cfg.Servers {
				names = append(names, n)
			}
			sort.Strings(names)
			var rows [][]string
			for _, n := range names {
				mark := "  "
				if n == cfg.Current {
					mark = ui.GreenS.Render("▶ ")
				}
				rows = append(rows, []string{mark + ui.Bold.Render(n), cfg.Servers[n].Target})
			}
			fmt.Println(ui.Table([]string{"  SERVER", "TARGET"}, rows))
			return nil
		},
	}
}

func serverUseCmd() *cobra.Command {
	return &cobra.Command{
		Use: "use <name>", Short: "Set the default server",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Servers[args[0]]; !ok {
				return fmt.Errorf("unknown server %q", args[0])
			}
			cfg.Current = args[0]
			if err := cfg.Save(); err != nil {
				return err
			}
			ui.Success("Default server is now %s", args[0])
			return nil
		},
	}
}

func serverRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use: "remove <name>", Short: "Forget a server (nothing is changed on it)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Servers[args[0]]; !ok {
				return fmt.Errorf("unknown server %q", args[0])
			}
			delete(cfg.Servers, args[0])
			if cfg.Current == args[0] {
				cfg.Current = ""
			}
			for dir, l := range cfg.Links {
				if l.Server == args[0] {
					delete(cfg.Links, dir)
				}
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			ui.Success("Forgot %s", args[0])
			return nil
		},
	}
}

func serverInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use: "info", Short: "Show server versions, resources and DokWalt's own footprint",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			var info api.ServerInfo
			if err := s.c.Get(cmd.Context(), "/v1/info", &info); err != nil {
				return err
			}
			if gf.json {
				return printJSON(info)
			}
			h := info.Host
			up := time.Duration(info.Uptime) * time.Second
			fmt.Println(ui.TitleS.Render("◆ "+s.name) + ui.MutedS.Render("  "+s.server.Target))
			fmt.Println()
			email := info.ACMEEmail
			if email == "" {
				email = ui.YellowS.Render("not set")
			}
			temp := ""
			if h.TempC > 0 {
				temp = fmt.Sprintf(" · %.0f°C", h.TempC)
			}
			fmt.Println(ui.KV(
				"Host", fmt.Sprintf("%s (linux/%s), up %s", info.Hostname, info.Version.Arch, roundUp(up)),
				"DokWalt", fmt.Sprintf("%s (API %s) · CLI %s", info.Version.Build, info.Version.APIVersion, Build),
				"Docker", fmt.Sprintf("%s · compose %s", info.DockerVersion, info.ComposeVer),
				"Apps", fmt.Sprint(info.Apps),
				"Resources", fmt.Sprintf("%d CPUs · %s / %s RAM · %s / %s disk%s", h.CPUs, ui.Bytes(h.MemUsed), ui.Bytes(h.MemTotal), ui.Bytes(h.DiskUsed), ui.Bytes(h.DiskTotal), temp),
				"Footprint", fmt.Sprintf("daemon %s · proxy %s", ui.Bytes(info.DaemonRSS), ui.Bytes(info.CaddyRSS))+
					ui.MutedS.Render(fmt.Sprintf("  (heap %s + %s; the rest is shared binary code)", ui.Bytes(info.DaemonHeap), ui.Bytes(info.CaddyHeap))),
				"Let's Encrypt", email,
			))
			return nil
		},
	}
}

func serverSettingsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "settings [key=value...]",
		Short:   "Show or change server settings (Let's Encrypt email, releases kept, drain, metrics retention)",
		Example: "  dokwalt server settings\n  dokwalt server settings keep_releases=10 drain=20s\n  dokwalt server settings acme_email=you@example.com",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if len(args) > 0 {
				set, err := parseAssignments(args)
				if err != nil {
					return err
				}
				if err := s.c.Post(cmd.Context(), "/v1/settings", set, nil); err != nil {
					return err
				}
				ui.Success("Settings updated")
			}
			var cur map[string]string
			if err := s.c.Get(cmd.Context(), "/v1/settings", &cur); err != nil {
				return err
			}
			if gf.json {
				return printJSON(cur)
			}
			show := func(v, empty string) string {
				if v == "" {
					return ui.YellowS.Render(empty)
				}
				return v
			}
			fmt.Println(ui.KV(
				"acme_email", show(cur["acme_email"], "not set")+ui.MutedS.Render("  Let's Encrypt expiry notices"),
				"keep_releases", cur["keep_releases"]+ui.MutedS.Render("  releases whose images stay on disk for rollbacks"),
				"drain", cur["drain"]+ui.MutedS.Render("  old version keeps running after the switch"),
				"metrics_retention_days", cur["metrics_retention_days"]+ui.MutedS.Render("  history kept for `dokwalt metrics`"),
			))
			return nil
		},
	}
}

func roundUp(d time.Duration) string {
	switch {
	case d > 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d > time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func serverUpgradeCmd() *cobra.Command {
	var binary string
	cmd := &cobra.Command{
		Use: "upgrade", Short: "Upgrade DokWalt on the server to this CLI's version (apps keep running)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name, err := serverName(cfg)
			if err != nil {
				return err
			}
			target := cfg.Servers[name].Target
			if strings.HasPrefix(target, "unix:") {
				return errors.New("upgrade a local server with `sudo dokwalt server bootstrap`")
			}
			if err := install(cmd, target, binary, ""); err != nil {
				return err
			}
			s, err := connectTo(cmd.Context(), cfg, name)
			if err != nil {
				return err
			}
			defer s.Close()
			ui.Success("%s now runs DokWalt %s — apps kept running during the upgrade", name, s.remote.Build)
			return nil
		},
	}
	cmd.Flags().StringVar(&binary, "binary", "", "path to a linux dokwalt binary (development)")
	return cmd
}

func serverBootstrapCmd() *cobra.Command {
	var o bootstrap.Options
	cmd := &cobra.Command{
		Use: "bootstrap", Short: "Install on this machine (runs on the server, as root)", Hidden: true,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return bootstrap.Run(o)
		},
	}
	cmd.Flags().StringVar(&o.User, "user", os.Getenv("SUDO_USER"), "user to grant access")
	cmd.Flags().StringVar(&o.Email, "email", "", "Let's Encrypt email")
	return cmd
}
