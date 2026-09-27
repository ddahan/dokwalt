// Package daemon is the long-running server process: a JSON API on a Unix
// socket plus background loops for reconciliation, metrics and alerts.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dokwalt/dokwalt/internal/alerts"
	"github.com/dokwalt/dokwalt/internal/compose"
	"github.com/dokwalt/dokwalt/internal/docker"
	"github.com/dokwalt/dokwalt/internal/engine"
	"github.com/dokwalt/dokwalt/internal/metrics"
	"github.com/dokwalt/dokwalt/internal/proxy"
	"github.com/dokwalt/dokwalt/internal/secrets"
	"github.com/dokwalt/dokwalt/internal/store"
)

const (
	DefaultDataDir = "/var/lib/dokwalt"
	DefaultSocket  = "/run/dokwalt/dokwalt.sock"
	Group          = "dokwalt"
)

type Options struct {
	DataDir      string
	Socket       string
	DockerSocket string
	Build        string
	Log          *slog.Logger
}

type Daemon struct {
	opts      Options
	store     *store.Store
	docker    *docker.Client
	proxy     *proxy.Manager
	engine    *engine.Engine
	alerts    *alerts.Manager
	collector *metrics.Collector
	http      *metrics.HTTPAggregator
	log       *slog.Logger
	started   time.Time
	heartbeat atomic.Int64
	kick      chan struct{}
	restarts  *alerts.RestartTracker
	wg        sync.WaitGroup
}

// goLoop runs a background loop that shutdown waits for.
func (d *Daemon) goLoop(fn func()) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		fn()
	}()
}

func Run(ctx context.Context, opts Options) error {
	if opts.DataDir == "" {
		opts.DataDir = DefaultDataDir
	}
	if opts.Socket == "" {
		opts.Socket = DefaultSocket
	}
	log := opts.Log
	if err := os.MkdirAll(opts.DataDir, 0o750); err != nil {
		return err
	}
	box, err := secrets.LoadOrCreate(filepath.Join(opts.DataDir, "secret.key"))
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(opts.DataDir, "dokwalt.db"), box)
	if err != nil {
		return err
	}
	defer st.Close()

	token := st.Setting("check_token", "")
	if token == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		token = hex.EncodeToString(b)
		_ = st.SetSetting("check_token", token)
	}
	dc := docker.New(opts.DockerSocket)
	px := proxy.New(filepath.Join(opts.DataDir, "caddy"), st.Setting("acme_email", ""), token)
	if err := px.Prepare(); err != nil {
		return err
	}
	d := &Daemon{
		opts: opts, store: st, docker: dc, proxy: px, log: log, started: time.Now(),
		engine:   engine.New(st, dc, px, opts.DataDir, log),
		alerts:   alerts.New(st, log),
		http:     metrics.NewHTTPAggregator(),
		kick:     make(chan struct{}, 1),
		restarts: alerts.NewRestartTracker(),
	}
	d.engine.Alert = d.alerts.Fire
	if v, err := strconv.Atoi(st.Setting("keep_releases", "5")); err == nil && v > 0 {
		d.engine.KeepReleases = v
	}
	if v, err := time.ParseDuration(st.Setting("drain", "10s")); err == nil {
		d.engine.Drain = v
	}
	d.collector = metrics.NewCollector(dc, st, d.http, opts.DataDir, log)
	if days, err := strconv.Atoi(st.Setting("metrics_retention_days", "7")); err == nil && days > 0 {
		d.collector.Retention = time.Duration(days) * 24 * time.Hour
	}
	d.http.OnTLS = func(e metrics.TLSEvent) {
		if e.OK {
			d.alerts.Fire("cert:"+e.Host, "Certificate error", "Certificate for "+e.Host+" obtained", false)
		} else {
			d.alerts.Fire("cert:"+e.Host, "Certificate error", "Could not get a certificate for "+e.Host+": "+e.Error+"\nRun `dokwalt doctor` to check DNS and ports.", true)
		}
	}
	d.heartbeat.Store(time.Now().Unix())

	ln, err := listen(opts.Socket)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: d.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("api server", "err", err)
		}
	}()
	log.Info("dokwalt daemon listening", "socket", opts.Socket, "data", opts.DataDir, "build", opts.Build)
	sdNotify("READY=1\nSTATUS=starting: waiting for docker")

	stop := make(chan struct{})
	go func() { _ = d.http.Listen(px.AccessSocket(), stop) }()
	go d.watchdog(ctx)
	d.goLoop(func() { d.mainLoop(ctx) })

	<-ctx.Done()
	sdNotify("STOPPING=1")
	close(stop)
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	// Let loops finish their current iteration before the store closes.
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdown.Done():
	}
	return nil
}

// listen creates the Unix socket: root:dokwalt 0660.
func listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	mode := os.FileMode(0o660)
	if g, err := user.LookupGroup(Group); err == nil {
		gid, _ := strconv.Atoi(g.Gid)
		_ = os.Chown(path, 0, gid)
	} else if os.Getuid() != 0 {
		mode = 0o600 // development: only the current user
	}
	if err := os.Chmod(path, mode); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// watchdog pings systemd while the main loop is alive. A main loop stuck for
// 10 minutes stops the pings and systemd restarts the daemon.
func (d *Daemon) watchdog(ctx context.Context) {
	iv := watchdogInterval()
	if iv == 0 {
		return
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if time.Since(time.Unix(d.heartbeat.Load(), 0)) < 10*time.Minute {
				sdNotify("WATCHDOG=1")
			}
		}
	}
}

// Kick asks the main loop to reconcile soon.
func (d *Daemon) Kick() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

func (d *Daemon) mainLoop(ctx context.Context) {
	if err := d.engine.WaitForDocker(ctx); err != nil {
		return
	}
	d.heartbeat.Store(time.Now().Unix())
	sdNotify("STATUS=recovering")
	d.engine.Recover(ctx)
	d.reconcile(ctx)
	sdNotify("STATUS=running")

	d.goLoop(func() { d.collector.Run(ctx) })
	d.goLoop(func() { d.watchEvents(ctx) })
	d.goLoop(func() { d.checksLoop(ctx) })

	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.kick:
			sleepCtx(ctx, 3*time.Second) // debounce bursts of events
		}
		d.heartbeat.Store(time.Now().Unix())
		d.reconcile(ctx)
	}
}

func (d *Daemon) reconcile(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()
	if err := d.engine.Reconcile(rctx); err != nil {
		d.log.Error("reconcile", "err", err)
	}
	d.refreshHosts()
}

func (d *Daemon) refreshHosts() {
	domains, err := d.store.Domains(0)
	if err != nil {
		return
	}
	var hosts []string
	for _, dm := range domains {
		hosts = append(hosts, dm.Hostname)
	}
	d.http.SetHosts(hosts)
}

// watchEvents reacts to containers dying: crash-loop alerts and fast repair.
func (d *Daemon) watchEvents(ctx context.Context) {
	tracker := d.restarts
	filters := map[string][]string{"type": {"container"}, "event": {"die", "oom", "destroy"}, "label": {compose.LabelApp}}
	for ctx.Err() == nil {
		err := d.docker.Events(ctx, filters, func(e docker.Event) {
			a := e.Actor.Attributes
			key := a[compose.LabelApp] + "/" + a[compose.LabelStage] + "/" + a[compose.LabelService]
			if e.Action == "die" && a[compose.LabelOneShot] != "true" {
				n := tracker.Died(key)
				if lim := int(d.alerts.Threshold("restarts")); lim > 0 && n >= lim {
					d.alerts.Fire("restarts:"+key, "Crash loop", fmt.Sprintf("%s restarted %d times in 10 minutes (exit code %s). `dokwalt logs -a %s`", key, n, a["exitCode"], a[compose.LabelApp]), true)
				}
			}
			if e.Action == "oom" {
				d.alerts.Fire("oom:"+key, "Out of memory", key+" was killed: out of memory", true)
			}
			d.Kick()
		})
		if ctx.Err() != nil {
			return
		}
		d.log.Warn("docker events stream ended, reconnecting", "err", err)
		_ = d.engine.WaitForDocker(ctx)
		d.Kick()
	}
}

// checksLoop evaluates alert conditions every minute.
func (d *Daemon) checksLoop(ctx context.Context) {
	prober := alerts.NewSiteProber()
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.alerts.CheckHost(d.collector.Host())
		for _, k := range d.restarts.Quiet() {
			d.alerts.Fire("restarts:"+k, "Crash loop", k+" is stable again", false)
		}
		stages, err := d.store.Stages(0)
		if err != nil {
			continue
		}
		deployed := map[int64]bool{}
		for _, st := range stages {
			if st.CurrentRelease > 0 && !d.engine.Stopped(st.ID) && !d.engine.IsDeploying(st.ID) {
				deployed[st.ID] = true
			}
		}
		domains, _ := d.store.Domains(0)
		for _, dm := range domains {
			if dm.RedirectTo == "" && deployed[dm.StageID] {
				pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				prober.Check(pctx, d.alerts, dm.Hostname)
				cancel()
			}
		}
	}
}

func sleepCtx(ctx context.Context, dur time.Duration) {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
