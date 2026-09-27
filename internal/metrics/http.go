package metrics

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/store"
)

// Latency histogram bucket upper bounds in milliseconds.
var buckets = []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000}

type hostAgg struct {
	requests       uint64
	s2, s3, s4, s5 uint64
	hist           [15]uint64
}

func (h *hostAgg) add(status int, ms float64) {
	h.requests++
	switch {
	case status >= 500:
		h.s5++
	case status >= 400:
		h.s4++
	case status >= 300:
		h.s3++
	default:
		h.s2++
	}
	i := len(buckets)
	for j, b := range buckets {
		if ms <= b {
			i = j
			break
		}
	}
	h.hist[i]++
}

func (h *hostAgg) quantile(q float64) float64 {
	if h.requests == 0 {
		return 0
	}
	target := uint64(q * float64(h.requests))
	var cum uint64
	for i, n := range h.hist {
		cum += n
		if cum >= target && n > 0 {
			if i < len(buckets) {
				return buckets[i]
			}
			return buckets[len(buckets)-1]
		}
	}
	return buckets[len(buckets)-1]
}

// TLSEvent is a certificate event reported by Caddy.
type TLSEvent struct {
	Host  string
	OK    bool
	Error string
}

// HTTPAggregator consumes Caddy's JSON access log over a Unix socket and
// aggregates per host. Nothing touches the disk.
type HTTPAggregator struct {
	mu     sync.Mutex
	minute map[string]*hostAgg // current minute
	window map[string]*hostAgg // last 10s, for live RPS
	rps    map[string]float64
	last   map[string]api.HTTPMetrics // last completed minute
	hosts  map[string]bool
	OnTLS  func(TLSEvent)
}

func NewHTTPAggregator() *HTTPAggregator {
	return &HTTPAggregator{
		minute: map[string]*hostAgg{}, window: map[string]*hostAgg{}, rps: map[string]float64{},
		last: map[string]api.HTTPMetrics{}, hosts: map[string]bool{}, OnTLS: func(TLSEvent) {},
	}
}

// SetHosts restricts aggregation to configured domains (scanners hitting
// random hosts must not grow memory).
func (a *HTTPAggregator) SetHosts(hosts []string) {
	m := map[string]bool{}
	for _, h := range hosts {
		m[h] = true
	}
	a.mu.Lock()
	a.hosts = m
	a.mu.Unlock()
}

type caddyLog struct {
	Logger     string  `json:"logger"`
	Level      string  `json:"level"`
	Msg        string  `json:"msg"`
	Status     int     `json:"status"`
	Duration   float64 `json:"duration"`
	Identifier string  `json:"identifier"`
	Error      string  `json:"error"`
	Request    struct {
		Host string `json:"host"`
	} `json:"request"`
}

func (a *HTTPAggregator) handleLine(b []byte) {
	var l caddyLog
	if json.Unmarshal(b, &l) != nil {
		return
	}
	if strings.HasPrefix(l.Logger, "tls") {
		switch {
		case l.Identifier != "" && strings.Contains(l.Msg, "certificate obtained successfully"):
			a.OnTLS(TLSEvent{Host: l.Identifier, OK: true})
		case l.Identifier != "" && l.Level == "error":
			a.OnTLS(TLSEvent{Host: l.Identifier, Error: l.Error})
		}
		return
	}
	host := l.Request.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.hosts[host] {
		return
	}
	for _, m := range []map[string]*hostAgg{a.minute, a.window} {
		agg := m[host]
		if agg == nil {
			agg = &hostAgg{}
			m[host] = agg
		}
		agg.add(l.Status, l.Duration*1000)
	}
}

// Listen accepts Caddy's log connections on a Unix socket.
func (a *HTTPAggregator) Listen(path string, stop <-chan struct{}) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	_ = os.Chmod(path, 0o666) // Caddy runs as root in its container, but be lenient
	go func() { <-stop; ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			return nil
		}
		go func() {
			defer c.Close()
			sc := bufio.NewScanner(c)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				a.handleLine(sc.Bytes())
			}
		}()
	}
}

// Tick10s rolls the live window. Call every 10 seconds.
func (a *HTTPAggregator) Tick10s() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rps = map[string]float64{}
	for h, agg := range a.window {
		a.rps[h] = float64(agg.requests) / 10
	}
	a.window = map[string]*hostAgg{}
}

// FlushMinute returns the completed minute's samples and starts a new one.
func (a *HTTPAggregator) FlushMinute(ts int64) []store.HTTPSample {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []store.HTTPSample
	a.last = map[string]api.HTTPMetrics{}
	for h, agg := range a.minute {
		s := store.HTTPSample{TS: ts, Hostname: h, Requests: agg.requests, S2: agg.s2, S3: agg.s3, S4: agg.s4, S5: agg.s5,
			P50: agg.quantile(0.5), P95: agg.quantile(0.95), P99: agg.quantile(0.99)}
		out = append(out, s)
		a.last[h] = api.HTTPMetrics{Hostname: h, Requests: s.Requests, S2xx: s.S2, S3xx: s.S3, S4xx: s.S4, S5xx: s.S5, P50: s.P50, P95: s.P95, P99: s.P99}
	}
	a.minute = map[string]*hostAgg{}
	return out
}

// Live returns per-host metrics: live RPS plus the last complete minute.
func (a *HTTPAggregator) Live() []api.HTTPMetrics {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []api.HTTPMetrics
	for h := range a.hosts {
		m := a.last[h]
		m.Hostname = h
		m.RPS = a.rps[h]
		out = append(out, m)
	}
	return out
}
