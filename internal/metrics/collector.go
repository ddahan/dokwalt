// Package metrics collects container, host and HTTP metrics cheaply: cgroup
// files instead of the Docker stats API, an in-memory ring for live views and
// one-minute aggregates in SQLite for history.
package metrics

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/compose"
	"github.com/ddahan/dokwalt/internal/docker"
	"github.com/ddahan/dokwalt/internal/store"
)

const (
	Interval    = 10 * time.Second
	historySize = 60 // 10 minutes of live history per stage
)

type prevSample struct {
	at  time.Time
	raw rawStats
}

type svcKey struct {
	app, stage, service string
}

type Collector struct {
	Docker    *docker.Client
	Store     *store.Store
	HTTP      *HTTPAggregator
	DiskPath  string
	Retention time.Duration
	Log       *slog.Logger

	mu       sync.RWMutex
	prev     map[string]prevSample // container id -> previous sample
	pids     map[string]int
	live     map[svcKey]*api.TopService
	host     api.HostMetrics
	history  map[string][]float64 // "app/stage" -> CPU% ring
	minute   map[svcKey][]api.LiveMetrics
	hostMin  []api.HostMetrics
	cpu      hostCPU
	throttle string
}

func NewCollector(dc *docker.Client, st *store.Store, http *HTTPAggregator, diskPath string, log *slog.Logger) *Collector {
	return &Collector{
		Docker: dc, Store: st, HTTP: http, DiskPath: diskPath, Retention: 7 * 24 * time.Hour, Log: log,
		prev: map[string]prevSample{}, pids: map[string]int{}, live: map[svcKey]*api.TopService{},
		history: map[string][]float64{}, minute: map[svcKey][]api.LiveMetrics{},
	}
}

// Run samples every Interval until ctx is done.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	lastMinute := time.Now().Truncate(time.Minute)
	lastPrune := time.Time{}
	lastThrottle := time.Time{}
	for {
		if time.Since(lastThrottle) > time.Minute {
			c.throttle = throttled()
			lastThrottle = time.Now()
		}
		c.sample(ctx)
		c.HTTP.Tick10s()
		now := time.Now()
		if m := now.Truncate(time.Minute); m.After(lastMinute) {
			c.flush(lastMinute.Unix())
			lastMinute = m
		}
		if time.Since(lastPrune) > time.Hour {
			_ = c.Store.PruneMetrics(now.Add(-c.Retention).Unix())
			lastPrune = now
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func rate(cur, prev uint64, dt float64) uint64 {
	if cur < prev || dt <= 0 {
		return 0
	}
	return uint64(float64(cur-prev) / dt)
}

func (c *Collector) sample(ctx context.Context) {
	host := hostMetrics(&c.cpu, c.DiskPath, c.throttle)
	cs, err := c.Docker.Containers(ctx, compose.LabelApp)
	if err != nil {
		c.Log.Debug("metrics: list containers", "err", err)
		return
	}
	now := time.Now()
	live := map[svcKey]*api.TopService{}
	seen := map[string]bool{}
	for _, ctr := range cs {
		if ctr.State != "running" {
			continue
		}
		l := ctr.Labels
		seen[ctr.ID] = true
		pid, ok := c.pids[ctr.ID]
		if !ok {
			if ci, err := c.Docker.Inspect(ctx, ctr.ID); err == nil {
				pid = ci.State.Pid
				c.pids[ctr.ID] = pid
			}
		}
		raw, ok := rawStats{}, false
		if dir := cgroupDir(ctr.ID); dir != "" {
			raw, ok = readCgroup(dir, pid)
		}
		if !ok {
			raw, ok = statsFromAPI(ctx, c.Docker, ctr.ID)
		}
		if !ok {
			continue
		}
		k := svcKey{l[compose.LabelApp], l[compose.LabelStage], l[compose.LabelService]}
		ts := live[k]
		if ts == nil {
			ts = &api.TopService{App: k.app, Stage: k.stage, Service: k.service}
			live[k] = ts
		}
		ts.Replicas++
		ts.Metrics.MemBytes += raw.memBytes
		ts.Metrics.MemLimit += raw.memLimit
		if p, ok := c.prev[ctr.ID]; ok {
			dt := now.Sub(p.at).Seconds()
			if raw.cpuUsec >= p.raw.cpuUsec && dt > 0 {
				ts.Metrics.CPUPct += float64(raw.cpuUsec-p.raw.cpuUsec) / (dt * 1e6) * 100
			}
			ts.Metrics.NetRx += rate(raw.netRx, p.raw.netRx, dt)
			ts.Metrics.NetTx += rate(raw.netTx, p.raw.netTx, dt)
			ts.Metrics.BlkR += rate(raw.blkR, p.raw.blkR, dt)
			ts.Metrics.BlkW += rate(raw.blkW, p.raw.blkW, dt)
		}
		c.prev[ctr.ID] = prevSample{at: now, raw: raw}
	}
	for id := range c.prev {
		if !seen[id] {
			delete(c.prev, id)
			delete(c.pids, id)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.live = live
	c.host = host
	c.hostMin = append(c.hostMin, host)
	perStage := map[string]float64{}
	for k, ts := range live {
		perStage[k.app+"/"+k.stage] += ts.Metrics.CPUPct
		c.minute[k] = append(c.minute[k], ts.Metrics)
	}
	for key, v := range perStage {
		h := append(c.history[key], v)
		if len(h) > historySize {
			h = h[len(h)-historySize:]
		}
		c.history[key] = h
	}
	for key := range c.history {
		if _, ok := perStage[key]; !ok {
			delete(c.history, key)
		}
	}
}

func (c *Collector) flush(ts int64) {
	c.mu.Lock()
	minute, hostMin := c.minute, c.hostMin
	c.minute, c.hostMin = map[svcKey][]api.LiveMetrics{}, nil
	c.mu.Unlock()

	stageIDs := map[string]int64{}
	if stages, err := c.Store.Stages(0); err == nil {
		for _, s := range stages {
			stageIDs[s.Key()] = s.ID
		}
	}
	var samples []store.ContainerSample
	for k, ms := range minute {
		id, ok := stageIDs[k.app+"/"+k.stage]
		if !ok || len(ms) == 0 {
			continue
		}
		var s store.ContainerSample
		for _, m := range ms {
			s.CPUPct += m.CPUPct
			s.MemBytes += m.MemBytes
			s.NetRx += m.NetRx
			s.NetTx += m.NetTx
			s.BlkR += m.BlkR
			s.BlkW += m.BlkW
		}
		n := uint64(len(ms))
		s.TS, s.StageID, s.Service = ts, id, k.service
		s.CPUPct /= float64(n)
		s.MemBytes, s.NetRx, s.NetTx, s.BlkR, s.BlkW = s.MemBytes/n, s.NetRx/n, s.NetTx/n, s.BlkR/n, s.BlkW/n
		samples = append(samples, s)
	}
	var host *store.HostSample
	if len(hostMin) > 0 {
		h := store.HostSample{TS: ts}
		for _, m := range hostMin {
			h.CPUPct += m.CPUPct
			h.MemUsed += m.MemUsed
			h.Load1 += m.Load1
			h.TempC += m.TempC
		}
		n := float64(len(hostMin))
		last := hostMin[len(hostMin)-1]
		h.CPUPct /= n
		h.MemUsed = uint64(float64(h.MemUsed) / n)
		h.Load1 /= n
		h.TempC /= n
		h.MemTotal, h.DiskUsed, h.DiskTotal = last.MemTotal, last.DiskUsed, last.DiskTotal
		host = &h
	}
	if err := c.Store.InsertMetrics(samples, c.HTTP.FlushMinute(ts), host); err != nil {
		c.Log.Warn("metrics: store", "err", err)
	}
}

// Snapshot returns the live view.
func (c *Collector) Snapshot() api.TopSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	snap := api.TopSnapshot{Time: time.Now(), Host: c.host, HTTP: c.HTTP.Live(), History: map[string][]float64{}}
	for _, ts := range c.live {
		snap.Services = append(snap.Services, *ts)
	}
	sort.Slice(snap.Services, func(i, j int) bool {
		a, b := snap.Services[i], snap.Services[j]
		if a.App != b.App {
			return a.App < b.App
		}
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		return a.Service < b.Service
	})
	sort.Slice(snap.HTTP, func(i, j int) bool { return snap.HTTP[i].Hostname < snap.HTTP[j].Hostname })
	for k, v := range c.history {
		snap.History[k] = append([]float64(nil), v...)
	}
	return snap
}

// Host returns the latest host metrics.
func (c *Collector) Host() api.HostMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.host
}

// Service returns live metrics of one service.
func (c *Collector) Service(app, stage, service string) *api.LiveMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if ts, ok := c.live[svcKey{app, stage, service}]; ok {
		m := ts.Metrics
		return &m
	}
	return nil
}
