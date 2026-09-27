// Package bootstrap installs DokWalt on a server. It runs as root on the
// server (`sudo dokwalt server bootstrap`) and is idempotent, so it doubles
// as the upgrade path.
package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dokwalt/dokwalt/internal/ui"
)

const (
	BinPath  = "/usr/local/bin/dokwalt"
	UnitPath = "/etc/systemd/system/dokwalt.service"
	Socket   = "/run/dokwalt/dokwalt.sock"
)

type Options struct {
	User  string // SSH user to grant access
	Email string // ACME email
}

const unit = `[Unit]
Description=DokWalt — Docker Compose apps with zero-downtime deploys
Documentation=https://github.com/dokwalt/dokwalt
After=docker.service network-online.target
Wants=network-online.target
Requires=docker.service

[Service]
Type=notify
ExecStart=/usr/local/bin/dokwalt daemon
Restart=always
RestartSec=2
# systemd restarts the daemon if its main loop hangs.
WatchdogSec=60
TimeoutStartSec=120
TimeoutStopSec=15
Environment=GOMEMLIMIT=40MiB
Environment=HOME=/root
RuntimeDirectory=dokwalt
RuntimeDirectoryMode=0755
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=full
ProtectKernelTunables=yes
ProtectControlGroups=no
LockPersonality=yes

[Install]
WantedBy=multi-user.target
`

func step(format string, a ...any) {
	fmt.Println(ui.AccentS.Render("→ ") + fmt.Sprintf(format, a...))
}
func ok(format string, a ...any) { fmt.Println(ui.OkIcon + " " + fmt.Sprintf(format, a...)) }
func warn(format string, a ...any) {
	fmt.Println(ui.WarnIcon + " " + ui.YellowS.Render(fmt.Sprintf(format, a...)))
}

func run(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

func quiet(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func Run(o Options) error {
	if runtime.GOOS != "linux" {
		return errors.New("bootstrap only runs on Linux")
	}
	if os.Geteuid() != 0 {
		return errors.New("bootstrap must run as root (sudo)")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemd is required")
	}
	fmt.Println(ui.TitleS.Render("◆ Installing DokWalt") + ui.MutedS.Render("  "+runtime.GOARCH))

	// Docker.
	if _, err := quiet("docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		if _, lerr := exec.LookPath("docker"); lerr != nil {
			step("Installing Docker (official get.docker.com script)")
			if err := run("sh", "-c", "command -v curl >/dev/null && curl -fsSL https://get.docker.com | sh || wget -qO- https://get.docker.com | sh"); err != nil {
				return fmt.Errorf("docker install failed: %w", err)
			}
		}
		_ = run("systemctl", "enable", "--now", "docker")
	}
	v, err := quiet("docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return fmt.Errorf("docker is not running: %s", v)
	}
	ok("Docker %s", v)
	if st, _ := quiet("systemctl", "is-enabled", "docker"); st != "enabled" {
		_ = run("systemctl", "enable", "docker")
	}
	if cv, err := quiet("docker", "compose", "version", "--short"); err == nil {
		ok("Docker Compose %s", cv)
	} else {
		step("Installing the Docker Compose plugin")
		if _, err := exec.LookPath("apt-get"); err == nil {
			if err := run("apt-get", "install", "-y", "docker-compose-plugin"); err != nil {
				return err
			}
		} else {
			return errors.New("install the docker compose plugin, then re-run")
		}
	}
	if err := configureDocker(); err != nil {
		return err
	}
	if mem, _ := quiet("docker", "info", "--format", "{{.MemoryLimit}}"); mem == "false" {
		warn("The memory cgroup is disabled: memory limits and metrics won't work.")
		fmt.Println(ui.MutedS.Render("  On a Raspberry Pi, append `cgroup_enable=memory cgroup_memory=1` to /boot/firmware/cmdline.txt and reboot."))
	}

	// Group & access.
	if _, err := user.LookupGroup("dokwalt"); err != nil {
		if err := run("groupadd", "--system", "dokwalt"); err != nil {
			return err
		}
	}
	if o.User != "" && o.User != "root" {
		if err := run("usermod", "-aG", "dokwalt,docker", o.User); err != nil {
			return err
		}
		ok("User %s can manage DokWalt (groups dokwalt, docker)", o.User)
	}

	// Binary.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, _ = filepath.EvalSymlinks(self); self != BinPath {
		if err := copyFile(self, BinPath); err != nil {
			return err
		}
	}
	ok("Installed %s", BinPath)
	if err := os.MkdirAll("/var/lib/dokwalt", 0o750); err != nil {
		return err
	}

	// Service.
	cur, _ := os.ReadFile(UnitPath)
	if string(cur) != unit {
		if err := os.WriteFile(UnitPath, []byte(unit), 0o644); err != nil {
			return err
		}
		if err := run("systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	if err := run("systemctl", "enable", "dokwalt"); err != nil {
		return err
	}
	if err := run("systemctl", "restart", "dokwalt"); err != nil {
		return err
	}
	step("Waiting for the daemon")
	if err := waitSocket(90 * time.Second); err != nil {
		out, _ := quiet("journalctl", "-u", "dokwalt", "-n", "30", "--no-pager")
		return fmt.Errorf("daemon did not start: %w\n%s", err, out)
	}
	ok("Daemon running (systemd unit dokwalt.service, starts at boot)")
	if o.Email != "" {
		if err := postSetting("acme_email", o.Email); err != nil {
			return err
		}
		ok("Let's Encrypt account email: %s", o.Email)
	}
	return nil
}

// configureDocker merges DokWalt's requirements into /etc/docker/daemon.json.
func configureDocker() error {
	const path = "/etc/docker/daemon.json"
	cfg := map[string]any{}
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	}
	changed := false
	if cfg["live-restore"] != true {
		cfg["live-restore"] = true
		changed = true
	}
	if _, ok := cfg["log-driver"]; !ok {
		cfg["log-driver"] = "local"
		changed = true
	}
	if !changed {
		ok("Docker already configured (live-restore, bounded logs)")
		return nil
	}
	step("Configuring Docker: live-restore (containers survive Docker restarts), bounded logs")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}
	// Reload first so live-restore is active before the restart applies the log driver.
	_ = run("systemctl", "reload", "docker")
	time.Sleep(time.Second)
	if err := run("systemctl", "restart", "docker"); err != nil {
		return err
	}
	for i := 0; i < 30; i++ {
		if _, err := quiet("docker", "info", "--format", "{{.ID}}"); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	ok("Docker configured")
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func socketClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", Socket)
	}}}
}

func waitSocket(timeout time.Duration) error {
	c := socketClient()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		resp, err := c.Get("http://dokwalt/v1/version")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		last = err
		time.Sleep(time.Second)
	}
	return last
}

func postSetting(k, v string) error {
	b, _ := json.Marshal(map[string]string{k: v})
	resp, err := socketClient().Post("http://dokwalt/v1/settings", "application/json", strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("settings: %s", msg)
	}
	return nil
}
