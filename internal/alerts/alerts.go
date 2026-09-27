// Package alerts sends rate-limited notifications to Discord and Slack
// webhooks, with a "resolved" message when a condition clears.
package alerts

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dokwalt/dokwalt/internal/api"
	"github.com/dokwalt/dokwalt/internal/store"
)

const Cooldown = 30 * time.Minute

// Default thresholds; override with `dokwalt alerts:set`.
var Defaults = map[string]string{
	"disk":        "90", // % used
	"memory":      "90", // % used
	"temperature": "80", // °C
	"restarts":    "3",  // container restarts within 10 minutes
}

type Manager struct {
	Store    *store.Store
	Log      *slog.Logger
	Hostname string
	client   *http.Client
	mu       sync.Mutex
}

func New(st *store.Store, log *slog.Logger) *Manager {
	h, _ := os.Hostname()
	return &Manager{Store: st, Log: log, Hostname: h, client: &http.Client{Timeout: 10 * time.Second}}
}

// Threshold returns a numeric threshold setting.
func (m *Manager) Threshold(name string) float64 {
	v := m.Store.Setting("alert."+name, Defaults[name])
	f, _ := strconv.ParseFloat(v, 64)
	return f
}

func (m *Manager) Thresholds() map[string]string {
	out := map[string]string{}
	for k, def := range Defaults {
		out[k] = m.Store.Setting("alert."+k, def)
	}
	return out
}

func (m *Manager) SetThreshold(name, value string) error {
	if _, ok := Defaults[name]; !ok {
		return fmt.Errorf("unknown alert setting %q (known: disk, memory, temperature, restarts)", name)
	}
	if _, err := strconv.ParseFloat(value, 64); err != nil {
		return fmt.Errorf("%s: %q is not a number", name, value)
	}
	return m.Store.SetSetting("alert."+name, value)
}

// Fire records a condition. firing=true sends an alert on transition (and
// again after the cooldown); firing=false sends "resolved" if it was firing.
func (m *Manager) Fire(key, title, message string, firing bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	states, err := m.Store.AlertStates()
	if err != nil {
		return
	}
	st := states[key]
	now := time.Now()
	switch {
	case firing && (!st.Firing || now.Sub(st.LastSent) > Cooldown):
		m.send("🔴 "+title, message)
		_ = m.Store.SetAlertState(key, store.AlertState{Firing: true, LastSent: now})
	case !firing && st.Firing:
		m.send("✅ Resolved: "+title, message)
		_ = m.Store.SetAlertState(key, store.AlertState{})
	}
}

// Firing lists keys currently in alert.
func (m *Manager) Firing() []string {
	states, _ := m.Store.AlertStates()
	var out []string
	for k, s := range states {
		if s.Firing {
			out = append(out, k)
		}
	}
	return out
}

func (m *Manager) send(title, message string) {
	chans, err := m.Store.AlertChannels()
	if err != nil || len(chans) == 0 {
		m.Log.Info("alert (no channel configured)", "title", title, "message", message)
		return
	}
	for _, c := range chans {
		if err := m.post(c, title, message); err != nil {
			m.Log.Warn("alert delivery failed", "channel", c.Kind, "err", err)
		}
	}
}

// Test sends a test message to every channel and returns delivery errors.
func (m *Manager) Test() []error {
	chans, err := m.Store.AlertChannels()
	if err != nil {
		return []error{err}
	}
	if len(chans) == 0 {
		return []error{fmt.Errorf("no alert channel configured — `dokwalt alerts:add discord <webhook-url>`")}
	}
	var errs []error
	for _, c := range chans {
		if err := m.post(c, "👋 DokWalt test alert", "Alerts from "+m.Hostname+" reach this channel."); err != nil {
			errs = append(errs, fmt.Errorf("%s #%d: %w", c.Kind, c.ID, err))
		}
	}
	return errs
}

func (m *Manager) post(c api.AlertChannel, title, message string) error {
	var body any
	text := fmt.Sprintf("%s — %s\n%s", title, m.Hostname, message)
	switch c.Kind {
	case "discord":
		body = map[string]any{"content": "**" + title + "** — `" + m.Hostname + "`\n" + message, "username": "DokWalt"}
	case "slack":
		body = map[string]any{"text": "*" + title + "* — `" + m.Hostname + "`\n" + message}
	default:
		body = map[string]any{"text": text}
	}
	b, _ := json.Marshal(body)
	resp, err := m.client.Post(c.URL, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// ---- periodic checks ----

// CheckHost evaluates host thresholds.
func (m *Manager) CheckHost(h api.HostMetrics) {
	if h.DiskTotal > 0 {
		pct := float64(h.DiskUsed) / float64(h.DiskTotal) * 100
		lim := m.Threshold("disk")
		m.Fire("host:disk", "Disk almost full", fmt.Sprintf("Disk is %.0f%% full (threshold %.0f%%). Try `docker system prune` or remove old apps.", pct, lim), lim > 0 && pct >= lim)
	}
	if h.MemTotal > 0 {
		pct := float64(h.MemUsed) / float64(h.MemTotal) * 100
		lim := m.Threshold("memory")
		m.Fire("host:memory", "Memory pressure", fmt.Sprintf("Memory is %.0f%% used (threshold %.0f%%).", pct, lim), lim > 0 && pct >= lim)
	}
	if h.TempC > 0 {
		lim := m.Threshold("temperature")
		m.Fire("host:temperature", "CPU temperature high", fmt.Sprintf("CPU at %.0f°C (threshold %.0f°C). Check cooling.", h.TempC, lim), lim > 0 && h.TempC >= lim)
	}
	m.Fire("host:throttled", "CPU throttling", "Raspberry Pi reports: "+h.Throttled+". Check power supply and cooling.", strings.HasPrefix(h.Throttled, "now:"))
}

// SiteProber checks public sites through the local Caddy (no hairpin NAT needed).
type SiteProber struct {
	client *http.Client
	fails  map[string]int
}

func NewSiteProber() *SiteProber {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &SiteProber{
		fails: map[string]int{},
		client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				// Always dial the local proxy, whatever DNS says.
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					_, port, _ := net.SplitHostPort(addr)
					return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
				},
				// Certificates are Caddy's business; we check the app answers.
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
				DisableKeepAlives: true,
			},
		},
	}
}

// Probe returns "" when the site answers, or a reason.
func (p *SiteProber) Probe(ctx context.Context, host string) string {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+host+"/", nil)
	req.Header.Set("User-Agent", "dokwalt-monitor")
	resp, err := p.client.Do(req)
	if err != nil {
		return err.Error()
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return ""
}

// Check probes a site and fires after two consecutive failures.
func (p *SiteProber) Check(ctx context.Context, m *Manager, host string) {
	reason := p.Probe(ctx, host)
	if reason == "" {
		p.fails[host] = 0
		m.Fire("site:"+host, "Site down", host+" answers again", false)
		return
	}
	p.fails[host]++
	if p.fails[host] >= 2 {
		m.Fire("site:"+host, "Site down", host+" is not answering: "+reason, true)
	}
}

// RestartTracker detects crash loops from Docker "die" events.
type RestartTracker struct {
	mu     sync.Mutex
	events map[string][]time.Time
}

func NewRestartTracker() *RestartTracker { return &RestartTracker{events: map[string][]time.Time{}} }

// Died records a container death and returns the count within 10 minutes.
func (r *RestartTracker) Died(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	var kept []time.Time
	for _, t := range r.events[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, time.Now())
	r.events[key] = kept
	return len(kept)
}

// Quiet returns keys with no deaths in the last 10 minutes and forgets them.
func (r *RestartTracker) Quiet() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	var out []string
	for k, ts := range r.events {
		if len(ts) == 0 || ts[len(ts)-1].Before(cutoff) {
			out = append(out, k)
			delete(r.events, k)
		}
	}
	return out
}
