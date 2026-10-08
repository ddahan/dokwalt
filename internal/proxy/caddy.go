// Package proxy manages the Caddy reverse proxy: it generates the complete
// JSON config from DokWalt's routes, persists it (so Caddy boots with the
// last-known-good config even before the daemon starts) and loads it
// atomically through Caddy's admin API on a Unix socket.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	Project       = "dokwalt-system"
	ContainerName = "dokwalt-caddy"
	Image         = "caddy:2-alpine"
	CheckPrefix   = "/.well-known/dokwalt-check/"
	// AccessLogger is the logger name Caddy uses for access logs.
	AccessLogger = "http.log.access.dokwalt"
)

// Route maps hostnames to upstream containers.
type Route struct {
	Hosts      []string
	Upstreams  []string // "container-name:port"
	RedirectTo string   // host to redirect to (308), keeps path
	App        string   // for the "not deployed" page
	Stopped    bool     // app stopped with `dokwalt stop`
}

type Manager struct {
	Dir        string // /var/lib/dokwalt/caddy
	Email      string
	CheckToken string
	admin      *http.Client
}

func New(dir, email, token string) *Manager {
	sock := filepath.Join(dir, "run", "admin.sock")
	return &Manager{
		Dir: dir, Email: email, CheckToken: token,
		admin: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}},
	}
}

func (m *Manager) ConfigPath() string   { return filepath.Join(m.Dir, "config", "caddy.json") }
func (m *Manager) AccessSocket() string { return filepath.Join(m.Dir, "run", "access.sock") }

// Prepare creates directories and a bootstrap config if none exists.
func (m *Manager) Prepare() error {
	for _, d := range []string{"config", "run"} {
		if err := os.MkdirAll(filepath.Join(m.Dir, d), 0o755); err != nil {
			return err
		}
	}
	if _, err := os.Stat(m.ConfigPath()); os.IsNotExist(err) {
		_, err := m.write(nil)
		return err
	}
	return nil
}

// SystemCompose is the compose project running Caddy.
func (m *Manager) SystemCompose() map[string]any {
	return map[string]any{
		"name": Project,
		"services": map[string]any{
			"caddy": map[string]any{
				"image":          Image,
				"container_name": ContainerName,
				"command":        []string{"caddy", "run", "--config", "/config/caddy.json"},
				"restart":        "always",
				"ports": []any{
					"80:80/tcp", "443:443/tcp", "443:443/udp",
				},
				"volumes": []any{
					"dokwalt-caddy-data:/data",
					m.Dir + "/config:/config",
					m.Dir + "/run:/run/caddy",
				},
				"labels":  map[string]string{"dokwalt.role": "system"},
				"logging": map[string]any{"driver": "local", "options": map[string]any{"max-size": "10m", "max-file": "3"}},
				// Keep Caddy lean on small machines.
				// No transparent huge pages: they inflate a Go heap's RSS several times.
				"environment": map[string]any{"GOMEMLIMIT": "96MiB", "GODEBUG": "disablethp=1"},
			},
		},
		"volumes": map[string]any{"dokwalt-caddy-data": map[string]any{"name": "dokwalt-caddy-data"}},
	}
}

// IsLocalName reports hostnames that can't get public certificates; Caddy
// serves them with its internal CA (useful for tests and LAN setups).
func IsLocalName(h string) bool {
	for _, s := range []string{".localhost", ".test", ".internal", ".local", ".lan", ".home.arpa"} {
		if h == strings.TrimPrefix(s, ".") || strings.HasSuffix(h, s) {
			return true
		}
	}
	return net.ParseIP(h) != nil
}

// Config builds the full Caddy JSON config.
func (m *Manager) Config(routes []Route) map[string]any {
	sort.Slice(routes, func(i, j int) bool { return strings.Join(routes[i].Hosts, ",") < strings.Join(routes[j].Hosts, ",") })
	var httpsRoutes []any
	var hosts, local []string
	for _, r := range routes {
		hosts = append(hosts, r.Hosts...)
		for _, h := range r.Hosts {
			if IsLocalName(h) {
				local = append(local, h)
			}
		}
		match := []any{map[string]any{"host": r.Hosts}}
		var handle []any
		switch {
		case r.RedirectTo != "":
			handle = []any{map[string]any{
				"handler":     "static_response",
				"status_code": 308,
				"headers":     map[string]any{"Location": []string{"https://" + r.RedirectTo + "{http.request.uri}"}},
			}}
		case len(r.Upstreams) == 0:
			body := fmt.Sprintf("%s is not running yet.\n", r.App)
			if r.Stopped {
				body = fmt.Sprintf("%s is temporarily unavailable for maintenance.\n", r.App)
			}
			handle = []any{map[string]any{
				"handler":     "static_response",
				"status_code": 503,
				"headers":     map[string]any{"Content-Type": []string{"text/plain; charset=utf-8"}, "Retry-After": []string{"30"}},
				"body":        body,
			}}
		default:
			ups := make([]any, 0, len(r.Upstreams))
			for _, u := range r.Upstreams {
				ups = append(ups, map[string]any{"dial": u})
			}
			handle = []any{map[string]any{
				"handler":        "reverse_proxy",
				"upstreams":      ups,
				"load_balancing": map[string]any{"selection_policy": map[string]any{"policy": "round_robin"}, "retries": 2, "try_duration": "5s"},
			}}
		}
		httpsRoutes = append(httpsRoutes, map[string]any{"match": match, "handle": handle, "terminal": true})
	}

	// Caddy inserts its HTTP→HTTPS redirects after the last route with a host
	// matcher, so the check route needs one to stay ahead of them.
	checkMatch := map[string]any{"path": []string{CheckPrefix + "*"}}
	if len(hosts) > 0 {
		sort.Strings(hosts)
		checkMatch["host"] = hosts
	}
	checkRoute := map[string]any{
		"match": []any{checkMatch},
		"handle": []any{map[string]any{
			"handler": "static_response", "status_code": 200, "body": m.CheckToken,
		}},
		"terminal": true,
	}

	policies := []any{}
	if len(local) > 0 {
		sort.Strings(local)
		policies = append(policies, map[string]any{"subjects": local, "issuers": []any{map[string]any{"module": "internal"}}})
	}
	acme := map[string]any{"module": "acme"}
	if m.Email != "" {
		acme["email"] = m.Email
	}
	policies = append(policies, map[string]any{"issuers": []any{acme}})

	return map[string]any{
		"admin": map[string]any{"listen": "unix//run/caddy/admin.sock"},
		"logging": map[string]any{"logs": map[string]any{
			"default": map[string]any{"exclude": []string{AccessLogger}},
			"dokwalt": map[string]any{
				"writer":  map[string]any{"output": "net", "address": "unix//run/caddy/access.sock", "soft_start": true, "dial_timeout": "2s"},
				"encoder": map[string]any{"format": "json"},
				"include": []string{AccessLogger, "tls"},
			},
		}},
		"apps": map[string]any{
			"http": map[string]any{"servers": map[string]any{
				"https": map[string]any{
					"listen": []string{":443"},
					"routes": orEmpty(httpsRoutes),
					"logs":   map[string]any{"default_logger_name": "dokwalt"},
				},
				"http": map[string]any{
					"listen": []string{":80"},
					"routes": []any{checkRoute},
				},
			}},
			"tls": map[string]any{"automation": map[string]any{"policies": policies}},
		},
	}
}

func orEmpty(s []any) []any {
	if s == nil {
		return []any{}
	}
	return s
}

// write persists the config atomically and returns its bytes.
func (m *Manager) write(routes []Route) ([]byte, error) {
	b, err := json.MarshalIndent(m.Config(routes), "", "  ")
	if err != nil {
		return nil, err
	}
	tmp := m.ConfigPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return nil, err
	}
	return b, os.Rename(tmp, m.ConfigPath())
}

// Hash of the config currently on disk.
func (m *Manager) FileHash() string {
	b, err := os.ReadFile(m.ConfigPath())
	if err != nil {
		return ""
	}
	return hashJSON(b)
}

// HashConfig hashes a config document the same way FileHash and LoadedHash do.
func HashConfig(cfg map[string]any) string {
	b, _ := json.Marshal(cfg)
	return hashJSON(b)
}

func hashJSON(b []byte) string {
	var v any
	if json.Unmarshal(b, &v) == nil {
		b, _ = json.Marshal(v) // canonical key order
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Apply writes the config and loads it into the running Caddy. If Caddy is
// down, the file is still written and will be used when it starts.
func (m *Manager) Apply(ctx context.Context, routes []Route) error {
	b, err := m.write(routes)
	if err != nil {
		return err
	}
	return m.load(ctx, b)
}

// Reload loads the on-disk config into Caddy (used by the reconciler).
func (m *Manager) Reload(ctx context.Context) error {
	b, err := os.ReadFile(m.ConfigPath())
	if err != nil {
		return err
	}
	return m.load(ctx, b)
}

func (m *Manager) load(ctx context.Context, b []byte) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/load", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.admin.Do(req)
	if err != nil {
		return fmt.Errorf("caddy admin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("caddy rejected config: %s", strings.TrimSpace(string(msg)))
	}
	return nil
}

// LoadedHash returns a hash of the config Caddy is running, "" if unreachable.
func (m *Manager) LoadedHash(ctx context.Context) string {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost/config/", nil)
	resp, err := m.admin.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return ""
	}
	return hashJSON(b)
}

// Reachable reports whether the admin API answers.
func (m *Manager) Reachable(ctx context.Context) bool {
	return m.LoadedHash(ctx) != ""
}
