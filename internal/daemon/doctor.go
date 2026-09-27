package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dokwalt/dokwalt/internal/api"
	"github.com/dokwalt/dokwalt/internal/compose"
	"github.com/dokwalt/dokwalt/internal/metrics"
	"github.com/dokwalt/dokwalt/internal/proxy"
	"github.com/dokwalt/dokwalt/internal/store"
)

// checkDomain proves end to end that the domain reaches this server's proxy
// by fetching a token Caddy serves over plain HTTP. Falls back to DNS to
// explain what's wrong.
func (d *Daemon) checkDomain(ctx context.Context, host string) string {
	if proxy.IsLocalName(host) {
		return "ok (local name, internal certificate)"
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+host+proxy.CheckPrefix+"probe", nil)
	if resp, err := client.Do(req); err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		if strings.TrimSpace(string(body)) == d.proxy.CheckToken {
			return "ok"
		}
	}
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil || len(ips) == 0 {
		return "no DNS record yet — create an A (and/or AAAA) record pointing to this server"
	}
	return fmt.Sprintf("DNS points to %s but this server's proxy didn't answer on port 80 — check the IP, firewall (80/443) and router port forwarding. (Testing from the same LAN? Your router may not support hairpin NAT: then this check can't succeed but the site may still work from outside.)", strings.Join(ips, ", "))
}

func (d *Daemon) getDoctor(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var checks []api.DoctorCheck
	add := func(name, status, msg, hint string) {
		checks = append(checks, api.DoctorCheck{Name: name, Status: status, Message: msg, Hint: hint})
	}

	// Docker.
	info, err := d.docker.Info(ctx)
	if err != nil {
		add("Docker", "fail", err.Error(), "sudo systemctl start docker")
		return writeJSON(w, checks)
	}
	v, _ := d.docker.Negotiate(ctx)
	add("Docker", "ok", fmt.Sprintf("Docker %s (API %s), %d CPUs, %s RAM", v.Version, v.APIVersion, info.NCPU, human(uint64(info.MemTotal))), "")
	if info.LiveRestore {
		add("Docker live-restore", "ok", "containers keep running while dockerd restarts or upgrades", "")
	} else {
		add("Docker live-restore", "warn", "disabled: restarting Docker restarts every site", `add "live-restore": true to /etc/docker/daemon.json, then sudo systemctl reload docker`)
	}
	if !info.MemoryLimit {
		add("Memory cgroup", "warn", "memory limits and accurate memory metrics are unavailable", "Raspberry Pi: append `cgroup_enable=memory cgroup_memory=1` to /boot/firmware/cmdline.txt and reboot")
	} else {
		add("Memory cgroup", "ok", "cgroup "+info.CgroupVersion+" ("+info.CgroupDriver+" driver)", "")
	}
	if out, err := exec.CommandContext(ctx, "docker", "compose", "version", "--short").Output(); err == nil {
		add("Compose plugin", "ok", "docker compose "+strings.TrimSpace(string(out)), "")
	} else {
		add("Compose plugin", "fail", "docker compose is not installed", "sudo apt install docker-compose-plugin")
	}
	for _, unit := range []string{"docker", "dokwalt"} {
		out, _ := exec.CommandContext(ctx, "systemctl", "is-enabled", unit).Output()
		switch s := strings.TrimSpace(string(out)); s {
		case "enabled":
			add("Start on boot: "+unit, "ok", unit+" starts at boot", "")
		case "":
			// not a systemd host (development)
		default:
			add("Start on boot: "+unit, "fail", unit+" is "+s+" — it won't start after a reboot", "sudo systemctl enable "+unit)
		}
	}

	// Host resources.
	h := d.collector.Host()
	if h.DiskTotal > 0 {
		pct := float64(h.DiskUsed) / float64(h.DiskTotal) * 100
		st := "ok"
		if pct >= d.alerts.Threshold("disk") {
			st = "warn"
		}
		add("Disk", st, fmt.Sprintf("%.0f%% used (%s of %s)", pct, human(h.DiskUsed), human(h.DiskTotal)), "free space: docker system prune --volumes (careful), or remove unused apps")
	}
	if h.MemTotal > 0 {
		pct := float64(h.MemUsed) / float64(h.MemTotal) * 100
		st := "ok"
		if pct >= d.alerts.Threshold("memory") {
			st = "warn"
		}
		add("Memory", st, fmt.Sprintf("%.0f%% used (%s of %s)", pct, human(h.MemUsed), human(h.MemTotal)), "")
	}
	if h.TempC > 0 {
		st := "ok"
		if h.TempC >= d.alerts.Threshold("temperature") {
			st = "warn"
		}
		add("Temperature", st, fmt.Sprintf("%.0f°C", h.TempC), "improve cooling (active cooler on a Raspberry Pi)")
	}
	if h.Throttled != "" {
		add("Throttling", "warn", h.Throttled, "use the official power supply and check cooling")
	}
	if out, err := exec.CommandContext(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value").Output(); err == nil {
		if strings.TrimSpace(string(out)) == "yes" {
			add("Clock", "ok", "synchronized with NTP", "")
		} else {
			add("Clock", "warn", "not synchronized — TLS certificates fail with a wrong clock", "sudo timedatectl set-ntp true")
		}
	}
	rss := metrics.ProcessRSS(os.Getpid())
	st := "ok"
	if rss > 60<<20 {
		st = "warn"
	}
	add("DokWalt daemon", st, fmt.Sprintf("build %s, %s RSS", d.opts.Build, human(rss)), "")

	// Proxy.
	ci, err := d.docker.Inspect(ctx, proxy.ContainerName)
	switch {
	case err != nil:
		add("Proxy (Caddy)", "fail", "container not found", "it is recreated automatically within a minute; check `docker logs dokwalt-caddy`")
	case !ci.State.Running:
		add("Proxy (Caddy)", "fail", "container is "+ci.State.Status, "docker logs dokwalt-caddy")
	case !d.proxy.Reachable(ctx):
		add("Proxy (Caddy)", "fail", "admin API not reachable", "docker logs dokwalt-caddy")
	default:
		msg := fmt.Sprintf("running, %s RSS", human(metrics.ProcessRSS(ci.State.Pid)))
		if d.proxy.LoadedHash(ctx) != d.proxy.FileHash() {
			add("Proxy (Caddy)", "warn", msg+", config out of date", "it is reloaded automatically within a minute")
		} else {
			add("Proxy (Caddy)", "ok", msg+", config loaded", "")
		}
	}
	if d.store.Setting("acme_email", "") == "" {
		add("Let's Encrypt email", "warn", "not set: you won't get expiry warnings from Let's Encrypt", "dokwalt server init --email you@example.com (or POST /v1/settings)")
	}

	// Apps.
	stages, _ := d.store.Stages(0)
	all, _ := d.docker.Containers(ctx, compose.LabelApp)
	for _, s := range stages {
		if s.CurrentRelease == 0 {
			continue
		}
		name := "App " + s.App
		if s.Name != api.DefaultStage {
			name += " (" + s.Name + ")"
		}
		status := d.engine.StageStatus(ctx, s, all)
		switch status {
		case "running", "stopped", "deploying":
			add(name, "ok", fmt.Sprintf("%s, v%d", status, s.CurrentRelease), "")
		default:
			add(name, "fail", status+problemContainers(ctx, d, s), "dokwalt logs -a "+s.App+stageFlag(s.Name))
		}
		if rels, _ := d.store.Releases(s.ID, 1); len(rels) == 1 && rels[0].Status == store.StatusFailed {
			add(name+" last deploy", "warn", fmt.Sprintf("v%d failed: %s", rels[0].Version, firstLine(rels[0].Error)), "dokwalt releases -a "+s.App+stageFlag(s.Name))
		}
	}
	domains, _ := d.store.Domains(0)
	for _, dm := range domains {
		res := d.checkDomain(ctx, dm.Hostname)
		if strings.HasPrefix(res, "ok") {
			add("Domain "+dm.Hostname, "ok", res, "")
		} else {
			add("Domain "+dm.Hostname, "warn", res, "")
		}
	}
	return writeJSON(w, checks)
}

func stageFlag(stage string) string {
	if stage == api.DefaultStage {
		return ""
	}
	return " -s " + stage
}

func problemContainers(ctx context.Context, d *Daemon, s store.Stage) string {
	cs, _ := d.engine.StageContainers(ctx, s)
	var bad []string
	for svc, list := range cs {
		for _, c := range list {
			if c.State != "running" || c.Health == "unhealthy" {
				bad = append(bad, fmt.Sprintf("%s %s", svc, c.Status))
			}
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return ": " + strings.Join(bad, "; ")
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func human(b uint64) string {
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
