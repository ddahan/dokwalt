// Package engine performs deployments: zero-downtime blue/green rollouts of
// stateless services, in-place updates of stateful ones, proxy switching,
// rollbacks, promotions and reconciliation after reboots.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/compose"
	"github.com/ddahan/dokwalt/internal/docker"
	"github.com/ddahan/dokwalt/internal/proxy"
	"github.com/ddahan/dokwalt/internal/store"
)

type Emit func(api.Event)

// AlertFunc reports a condition to the alerting subsystem.
type AlertFunc func(key, title, message string, firing bool)

type Engine struct {
	Store     *store.Store
	Docker    *docker.Client
	Proxy     *proxy.Manager
	DataDir   string
	DockerBin string
	Log       *slog.Logger
	Alert     AlertFunc

	KeepReleases int
	Drain        time.Duration

	lock      chan struct{}
	mu        sync.Mutex
	deploying map[int64]bool  // stage id -> deploy in progress
	draining  map[string]bool // old color projects finishing in-flight requests
}

func New(st *store.Store, dc *docker.Client, px *proxy.Manager, dataDir string, log *slog.Logger) *Engine {
	return &Engine{
		Store: st, Docker: dc, Proxy: px, DataDir: dataDir, DockerBin: "docker", Log: log,
		KeepReleases: 5, Drain: 10 * time.Second,
		lock:      make(chan struct{}, 1),
		deploying: map[int64]bool{},
		draining:  map[string]bool{},
		Alert:     func(string, string, string, bool) {},
	}
}

// acquire serializes mutations: one deploy at a time keeps small machines responsive.
func (e *Engine) acquire(ctx context.Context) (func(), error) {
	select {
	case e.lock <- struct{}{}:
		return func() { <-e.lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *Engine) tryAcquire() (func(), bool) {
	select {
	case e.lock <- struct{}{}:
		return func() { <-e.lock }, true
	default:
		return nil, false
	}
}

func (e *Engine) IsDeploying(stageID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deploying[stageID]
}

func (e *Engine) setDeploying(stageID int64, v bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v {
		e.deploying[stageID] = true
	} else {
		delete(e.deploying, stageID)
	}
}

func ev(step, status, msg string) api.Event {
	return api.Event{Time: time.Now(), Step: step, Status: status, Message: msg}
}

// DeploySpec describes what to run.
type DeploySpec struct {
	Compose     map[string]any
	Images      map[string]api.Image
	Description string
	GitSHA      string
	ProjectDir  string
}

// Deploy creates a release for app/stage and rolls it out.
func (e *Engine) Deploy(ctx context.Context, app, stage string, spec DeploySpec, emit Emit) (int, error) {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	st, err := e.Store.GetStage(app, stage)
	if err != nil {
		return 0, err
	}
	return e.deployLocked(ctx, st, spec, emit)
}

func (e *Engine) plan(st store.Stage, spec DeploySpec, release int) (compose.Plan, map[string]string, error) {
	cfg, err := e.Store.Config(st.ID)
	if err != nil {
		return compose.Plan{}, nil, err
	}
	ovs, err := e.Store.Overrides(st.ID)
	if err != nil {
		return compose.Plan{}, nil, err
	}
	stateful := map[string]*bool{}
	for k, o := range ovs {
		stateful[k] = o.Stateful
	}
	refs := map[string]string{}
	for k, img := range spec.Images {
		refs[k] = img.Ref
	}
	plan, err := compose.Transform(compose.Input{
		App: st.App, Stage: st.Name, Release: release, Compose: spec.Compose, Images: refs,
		Stateful: stateful, ConfigKeys: store.SortedKeys(cfg), ProjectDir: spec.ProjectDir,
	})
	return plan, cfg, err
}

func (e *Engine) deployLocked(ctx context.Context, st store.Stage, spec DeploySpec, emit Emit) (int, error) {
	e.setDeploying(st.ID, true)
	defer e.setDeploying(st.ID, false)

	emit(ev("validate", "start", "Validating compose file"))
	plan, _, err := e.plan(st, spec, 0)
	if err != nil {
		return 0, err
	}
	for svc, img := range spec.Images {
		ok, err := e.Docker.ImageExists(ctx, img.Ref)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, fmt.Errorf("image %s for service %s is not on the server (was it pruned? keep fewer releases or redeploy)", img.Ref, svc)
		}
	}
	for _, w := range plan.Warnings {
		emit(ev("validate", "warn", w))
	}
	for _, m := range plan.Missing {
		emit(ev("validate", "warn", fmt.Sprintf("${%s} is used in the compose file but not set — `dokwalt config:set %s=...`", m, m)))
	}
	domains, err := e.Store.Domains(st.ID)
	if err != nil {
		return 0, err
	}
	for _, d := range domains {
		if d.RedirectTo == "" {
			if _, ok := plan.Service(d.Service); !ok {
				emit(ev("validate", "warn", fmt.Sprintf("domain %s points to service %q which is not in the compose file", d.Hostname, d.Service)))
			}
		}
	}
	var roles []string
	for _, s := range plan.Services {
		kind := "stateless"
		if s.Stateful {
			kind = "stateful (" + s.Reason + ")"
		}
		if s.OneShot {
			kind = "one-shot job"
		}
		roles = append(roles, s.Name+": "+kind)
	}
	emit(ev("validate", "done", "Services — "+strings.Join(roles, ", ")))

	color := ""
	if plan.App != nil {
		color = "blue"
		if st.ActiveColor == "blue" {
			color = "green"
		}
	}
	rel := &store.Release{
		StageID: st.ID, Status: store.StatusDeploying, Images: spec.Images, Compose: spec.Compose,
		ConfigVersion: st.ConfigVersion, Color: color, Description: spec.Description, GitSHA: spec.GitSHA,
	}
	if err := e.Store.CreateRelease(rel); err != nil {
		return 0, err
	}
	emit(api.Event{Time: time.Now(), Step: "release", Status: "done", Message: fmt.Sprintf("Created release v%d", rel.Version), Release: rel.Version})
	plan, cfg, err := e.plan(st, spec, rel.Version)
	if err == nil {
		err = e.rollout(ctx, st, rel, plan, cfg, emit)
	}
	if err != nil {
		_ = e.Store.FinishRelease(rel.ID, store.StatusFailed, err.Error())
		e.Alert("deploy:"+st.Key(), "Deploy failed", fmt.Sprintf("%s v%d failed: %s", st.Key(), rel.Version, firstLine(err.Error())), true)
		return rel.Version, err
	}
	_ = e.Store.FinishRelease(rel.ID, store.StatusSucceeded, "")
	_ = e.Store.SupersedeOthers(st.ID, rel.Version)
	e.Alert("deploy:"+st.Key(), "Deploy succeeded", fmt.Sprintf("%s v%d is live", st.Key(), rel.Version), false)
	go e.pruneImages(context.Background(), st.App)
	return rel.Version, nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func (e *Engine) rollout(ctx context.Context, st store.Stage, rel *store.Release, plan compose.Plan, cfg map[string]string, emit Emit) error {
	netName := compose.Network(st.App, st.Name)
	labels := map[string]string{compose.LabelApp: st.App, compose.LabelStage: st.Name}
	if err := e.ensureProxy(ctx); err != nil {
		return fmt.Errorf("reverse proxy: %w", err)
	}
	if err := e.Docker.EnsureNetwork(ctx, netName, labels); err != nil {
		return err
	}
	if err := e.Docker.ConnectNetwork(ctx, netName, proxy.ContainerName); err != nil {
		return fmt.Errorf("connect proxy to %s: %w", netName, err)
	}
	for _, v := range plan.Volumes {
		if err := e.Docker.CreateVolume(ctx, v, labels); err != nil {
			return err
		}
	}
	logLine := func(step string) func(string) {
		return func(l string) { emit(ev(step, "log", l)) }
	}

	dataProject := compose.DataProject(st.App, st.Name)
	if plan.Data != nil {
		emit(ev("data", "start", "Updating stateful services"))
		file, err := e.composeFile(st.App, st.Name, dataProject, plan.Data)
		if err != nil {
			return err
		}
		before := containerIDs(e.projectContainers(ctx, dataProject))
		if err := e.compose(ctx, dataProject, file, cfg, logLine("data"), "up", "-d", "--remove-orphans"); err != nil {
			return err
		}
		if after := containerIDs(e.projectContainers(ctx, dataProject)); after != before {
			// Something was (re)created: wait until it's ready and stable.
			if err := e.waitReady(ctx, dataProject, 3*time.Minute, "data", emit); err != nil {
				return err
			}
			emit(ev("data", "done", "Stateful services updated"))
		} else {
			emit(ev("data", "done", "Stateful services unchanged"))
		}
	} else if cs, _ := e.projectContainers(ctx, dataProject); len(cs) > 0 {
		emit(ev("data", "warn", "No stateful services anymore: stopping the data project (volumes are kept)"))
		_ = e.compose(ctx, dataProject, "", nil, logLine("data"), "down", "--remove-orphans")
	}

	var newProject string
	if plan.App != nil {
		newProject = compose.ColorProject(st.App, st.Name, rel.Color)
		doc, err := compose.RenderApp(plan, st.App, st.Name, rel.Color)
		if err != nil {
			return err
		}
		file, err := e.composeFile(st.App, st.Name, newProject, doc)
		if err != nil {
			return err
		}
		if cs, _ := e.projectContainers(ctx, newProject); len(cs) > 0 {
			_ = e.compose(ctx, newProject, "", nil, nil, "down", "--remove-orphans", "--timeout", "10")
		}
		emit(ev("start", "start", fmt.Sprintf("Starting v%d (%s)", rel.Version, rel.Color)))
		fail := func(err error) error {
			emit(ev("rollback", "start", "Stopping the new version; the current version keeps serving"))
			_ = e.compose(context.Background(), newProject, "", nil, nil, "down", "--remove-orphans", "--timeout", "5")
			return err
		}
		if err := e.compose(ctx, newProject, file, cfg, logLine("start"), "up", "-d", "--remove-orphans"); err != nil {
			e.emitFailedJobs(ctx, newProject, emit)
			return fail(err)
		}
		if err := e.waitReady(ctx, newProject, 3*time.Minute, "start", emit); err != nil {
			return fail(err)
		}
		emit(ev("start", "done", "Containers running"))
		if err := e.healthCheck(ctx, st, plan, newProject, emit); err != nil {
			return fail(err)
		}
	}

	emit(ev("switch", "start", "Switching traffic"))
	routes, err := e.Routes(ctx, map[int64]string{st.ID: rel.Color})
	if err == nil {
		err = e.Proxy.Apply(ctx, routes)
	}
	if err != nil {
		if newProject != "" {
			_ = e.compose(context.Background(), newProject, "", nil, nil, "down", "--remove-orphans")
		}
		if old, rerr := e.Routes(ctx, nil); rerr == nil {
			_ = e.Proxy.Apply(ctx, old)
		}
		return fmt.Errorf("switch traffic: %w", err)
	}
	if err := e.Store.SetStageActive(st.ID, rel.Color, rel.Version); err != nil {
		return err
	}
	emit(ev("switch", "done", fmt.Sprintf("v%d is live", rel.Version)))

	if old := st.ActiveColor; old != "" && old != rel.Color {
		emit(ev("drain", "done", fmt.Sprintf("Previous version stops in %s (in-flight requests finish)", e.Drain)))
		project := compose.ColorProject(st.App, st.Name, old)
		e.setDraining(project, true)
		go e.drain(st.ID, project, old)
	}
	return nil
}

// drain stops an old color project once it is no longer active.
func (e *Engine) setDraining(project string, v bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v {
		e.draining[project] = true
	} else {
		delete(e.draining, project)
	}
}

func (e *Engine) isDraining(project string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.draining[project]
}

func (e *Engine) drain(stageID int64, project, color string) {
	defer e.setDraining(project, false)
	time.Sleep(e.Drain)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	unlock, err := e.acquire(ctx)
	if err != nil {
		return
	}
	defer unlock()
	st, err := e.Store.GetStageByID(stageID)
	if err == nil && st.ActiveColor == color {
		return // became active again (e.g. rollback) meanwhile
	}
	if err := e.compose(ctx, project, "", nil, nil, "down", "--remove-orphans", "--timeout", "20"); err != nil {
		e.Log.Warn("drain failed", "project", project, "err", err)
	}
}

// containerIDs returns a comparable fingerprint of running containers.
func containerIDs(cs []docker.ContainerSummary, err error) string {
	if err != nil {
		return "error"
	}
	var ids []string
	for _, c := range cs {
		if c.State == "running" || c.Labels[compose.LabelOneShot] == "true" {
			ids = append(ids, c.ID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// projectContainers lists containers of a compose project.
func (e *Engine) projectContainers(ctx context.Context, project string) ([]docker.ContainerSummary, error) {
	return e.Docker.Containers(ctx, "com.docker.compose.project="+project)
}

// waitReady waits until every container of a project is running (and healthy
// when it has a health check) or, for one-shot jobs, exited successfully.
func (e *Engine) waitReady(ctx context.Context, project string, timeout time.Duration, step string, emit Emit) error {
	deadline := time.Now().Add(timeout)
	start := time.Now()
	lastMsg := ""
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for containers to become ready (%s)", timeout, lastMsg)
		}
		cs, err := e.projectContainers(ctx, project)
		if err != nil {
			return err
		}
		if len(cs) == 0 {
			if time.Since(start) > 20*time.Second {
				return fmt.Errorf("no containers were created for %s", project)
			}
			sleepCtx(ctx, time.Second)
			continue
		}
		ready := true
		var waiting []string
		restarts := map[string]int{}
		for _, c := range cs {
			ci, err := e.Docker.Inspect(ctx, c.ID)
			if err != nil {
				return err
			}
			name := c.Labels["com.docker.compose.service"]
			restarts[c.ID] = ci.RestartCount
			oneShot := c.Labels[compose.LabelOneShot] == "true"
			switch {
			case oneShot && ci.State.Status == "exited" && ci.State.ExitCode == 0:
			case oneShot && ci.State.Status == "exited":
				e.emitLogs(ctx, c, ci, emit, step)
				return fmt.Errorf("job %s failed with exit code %d", name, ci.State.ExitCode)
			case ci.State.Status == "running" && (ci.Health() == "" || ci.Health() == "healthy"):
			case ci.State.Status == "running" && ci.Health() == "starting":
				ready = false
				waiting = append(waiting, name+" health check")
			case ci.Health() == "unhealthy":
				e.emitLogs(ctx, c, ci, emit, step)
				return fmt.Errorf("service %s is unhealthy", name)
			case ci.State.Restarting || ci.State.Status == "exited" || ci.State.Status == "dead":
				e.emitLogs(ctx, c, ci, emit, step)
				msg := fmt.Sprintf("service %s exited with code %d", name, ci.State.ExitCode)
				if ci.State.OOMKilled {
					msg += " (out of memory)"
				}
				return errors.New(msg)
			default:
				ready = false
				waiting = append(waiting, name+" "+ci.State.Status)
			}
		}
		msg := "waiting for " + strings.Join(waiting, ", ")
		if !ready && msg != lastMsg {
			emit(ev(step, "progress", msg))
		}
		lastMsg = msg
		if ready {
			// Stability: nothing restarts during a short grace period.
			sleepCtx(ctx, 3*time.Second)
			for _, c := range cs {
				ci, err := e.Docker.Inspect(ctx, c.ID)
				if err != nil {
					return err
				}
				if c.Labels[compose.LabelOneShot] == "true" {
					continue
				}
				if ci.RestartCount != restarts[c.ID] || ci.State.Status != "running" {
					e.emitLogs(ctx, c, ci, emit, step)
					return fmt.Errorf("service %s is crash-looping", c.Labels["com.docker.compose.service"])
				}
			}
			return nil
		}
		sleepCtx(ctx, time.Second)
	}
}

func (e *Engine) emitLogs(ctx context.Context, c docker.ContainerSummary, ci docker.ContainerJSON, emit Emit, step string) {
	svc := c.Labels["com.docker.compose.service"]
	emit(api.Event{Time: time.Now(), Step: step, Status: "warn", Service: svc, Message: "Last log lines of " + svc + ":"})
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = e.Docker.Logs(lctx, c.ID, ci.Config.Tty, docker.LogOptions{Tail: "30"}, func(_ string, _ time.Time, line string) {
		emit(api.Event{Time: time.Now(), Step: step, Status: "log", Service: svc, Message: line})
	})
}

// emitFailedJobs shows the logs of one-shot jobs (migrations) that exited
// non-zero: compose itself only reports "didn't complete successfully".
func (e *Engine) emitFailedJobs(ctx context.Context, project string, emit Emit) {
	cs, err := e.projectContainers(ctx, project)
	if err != nil {
		return
	}
	for _, c := range cs {
		if c.Labels[compose.LabelOneShot] != "true" {
			continue
		}
		ci, err := e.Docker.Inspect(ctx, c.ID)
		if err == nil && ci.State.Status == "exited" && ci.State.ExitCode != 0 {
			e.emitLogs(ctx, c, ci, emit, "start")
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// healthCheck probes public stateless services over HTTP before they get traffic.
func (e *Engine) healthCheck(ctx context.Context, st store.Stage, plan compose.Plan, project string, emit Emit) error {
	domains, err := e.Store.Domains(st.ID)
	if err != nil {
		return err
	}
	ovs, _ := e.Store.Overrides(st.ID)
	checked := map[string]bool{}
	netName := compose.Network(st.App, st.Name)
	for _, d := range domains {
		info, ok := plan.Service(d.Service)
		if d.RedirectTo != "" || !ok || info.Stateful || checked[d.Service] {
			continue
		}
		checked[d.Service] = true
		ov := ovs[d.Service]
		path, strict := "/", false
		if ov.HealthPath != "" {
			path, strict = ov.HealthPath, true
		}
		timeout := 60 * time.Second
		if ov.HealthTimeout > 0 {
			timeout = time.Duration(ov.HealthTimeout) * time.Second
		}
		cs, err := e.Docker.Containers(ctx, "com.docker.compose.project="+project, compose.LabelService+"="+d.Service)
		if err != nil {
			return err
		}
		port := compose.ResolvePort(d.Port, info.Ports)
		emit(ev("health", "start", fmt.Sprintf("Checking %s on port %d (GET %s)", d.Service, port, path)))
		for _, c := range cs {
			ci, err := e.Docker.Inspect(ctx, c.ID)
			if err != nil {
				return err
			}
			ip := ci.NetworkSettings.Networks[netName].IPAddress
			if ip == "" {
				return fmt.Errorf("%s has no address on %s", c.Name(), netName)
			}
			url := "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + path
			if err := probe(ctx, url, strict, timeout); err != nil {
				e.emitLogs(ctx, c, ci, emit, "health")
				hint := "declare it in the compose file (ports/expose) or pass --port to domains:add"
				if d.Port > 0 {
					hint = "fix the port with `dokwalt domains:remove` + `domains:add --port`"
				}
				return fmt.Errorf("health check failed for %s: %w — is the app listening on port %d? (%s)", d.Service, err, port, hint)
			}
		}
		emit(ev("health", "done", d.Service+" is healthy"))
	}
	return nil
}

func probe(ctx context.Context, url string, strict bool, timeout time.Duration) error {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Header.Set("User-Agent", "dokwalt-healthcheck")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 && (!strict || resp.StatusCode < 400) {
				return nil
			}
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			last = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sleepCtx(ctx, time.Second)
	}
	return last
}

// Routes computes the proxy routes from domains and running containers.
// overrides forces the active color of some stages (used during a switch).
func (e *Engine) Routes(ctx context.Context, overrides map[int64]string) ([]proxy.Route, error) {
	domains, err := e.Store.Domains(0)
	if err != nil {
		return nil, err
	}
	stages, err := e.Store.Stages(0)
	if err != nil {
		return nil, err
	}
	byID := map[int64]store.Stage{}
	for _, s := range stages {
		byID[s.ID] = s
	}
	cs, err := e.Docker.Containers(ctx, compose.LabelApp)
	if err != nil {
		return nil, err
	}
	var routes []proxy.Route
	for _, d := range domains {
		st := byID[d.StageID]
		if d.RedirectTo != "" {
			routes = append(routes, proxy.Route{Hosts: []string{d.Hostname}, RedirectTo: d.RedirectTo, App: st.App})
			continue
		}
		color := st.ActiveColor
		if c, ok := overrides[st.ID]; ok {
			color = c
		}
		r := proxy.Route{Hosts: []string{d.Hostname}, App: st.App, Stopped: e.Stopped(st.ID)}
		if !r.Stopped {
			for _, c := range cs {
				l := c.Labels
				if l[compose.LabelApp] != st.App || l[compose.LabelStage] != st.Name || l[compose.LabelService] != d.Service || c.State != "running" {
					continue
				}
				if l[compose.LabelRole] == "data" || l[compose.LabelColor] == color {
					port := compose.ResolvePort(d.Port, compose.PortsFromLabel(l[compose.LabelPorts]))
					r.Upstreams = append(r.Upstreams, c.Name()+":"+strconv.Itoa(port))
				}
			}
			sort.Strings(r.Upstreams)
		}
		routes = append(routes, r)
	}
	return routes, nil
}

// ApplyRoutes recomputes and loads the proxy config.
func (e *Engine) ApplyRoutes(ctx context.Context) error {
	routes, err := e.Routes(ctx, nil)
	if err != nil {
		return err
	}
	return e.Proxy.Apply(ctx, routes)
}

// ensureProxy makes sure the Caddy container runs and its admin API answers.
func (e *Engine) ensureProxy(ctx context.Context) error {
	if err := e.Proxy.Prepare(); err != nil {
		return err
	}
	cs, err := e.Docker.Containers(ctx, "com.docker.compose.project="+proxy.Project)
	if err != nil {
		return err
	}
	running := false
	for _, c := range cs {
		if c.State == "running" {
			running = true
		}
	}
	// Recreate Caddy only when it isn't running or its definition changed
	// (e.g. after an upgrade): that costs all sites a ~1s blip.
	path := filepath.Join(e.DataDir, "system", proxy.Project+".json")
	before, _ := os.ReadFile(path)
	file, err := e.composeFile("", "", proxy.Project, e.Proxy.SystemCompose())
	if err != nil {
		return err
	}
	after, _ := os.ReadFile(file)
	if !running || string(before) != string(after) {
		if err := e.compose(ctx, proxy.Project, file, nil, nil, "up", "-d"); err != nil {
			return err
		}
	}
	for i := 0; i < 30; i++ {
		if e.Proxy.Reachable(ctx) {
			return nil
		}
		sleepCtx(ctx, 500*time.Millisecond)
	}
	return errors.New("caddy admin API not reachable")
}

// ---- rollback, promote, config releases ----

// Rollback redeploys an earlier release (the previous successful one when version is 0).
func (e *Engine) Rollback(ctx context.Context, app, stage string, version int, withConfig bool, emit Emit) (int, error) {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	st, err := e.Store.GetStage(app, stage)
	if err != nil {
		return 0, err
	}
	if version == 0 {
		rels, err := e.Store.Releases(st.ID, 50)
		if err != nil {
			return 0, err
		}
		for _, r := range rels {
			if r.Version < st.CurrentRelease && (r.Status == store.StatusSucceeded || r.Status == store.StatusSuperseded) {
				version = r.Version
				break
			}
		}
		if version == 0 {
			return 0, errors.New("no earlier successful release to roll back to")
		}
	}
	rel, err := e.Store.GetRelease(st.ID, version)
	if err != nil {
		return 0, err
	}
	if withConfig {
		cfg, err := e.Store.ConfigSnapshot(st.ID, rel.ConfigVersion)
		if err != nil {
			return 0, fmt.Errorf("config of v%d: %w", version, err)
		}
		if _, err := e.Store.ReplaceConfig(st.ID, cfg); err != nil {
			return 0, err
		}
		emit(ev("config", "done", fmt.Sprintf("Restored config from v%d", version)))
		st, _ = e.Store.GetStage(app, stage)
	}
	desc := fmt.Sprintf("Rollback to v%d", version)
	return e.deployLocked(ctx, st, DeploySpec{Compose: rel.Compose, Images: rel.Images, Description: desc, GitSHA: rel.GitSHA}, emit)
}

// Promote releases staging's current images to production. No build, no transfer.
func (e *Engine) Promote(ctx context.Context, app string, emit Emit) (int, error) {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	staging, err := e.Store.GetStage(app, api.StagingStage)
	if err != nil {
		return 0, err
	}
	if staging.CurrentRelease == 0 {
		return 0, errors.New("staging has no release yet — deploy to staging first (`dokwalt deploy -s staging`)")
	}
	rel, err := e.Store.GetRelease(staging.ID, staging.CurrentRelease)
	if err != nil {
		return 0, err
	}
	prod, err := e.Store.GetStage(app, api.DefaultStage)
	if err != nil {
		return 0, err
	}
	emit(ev("promote", "done", fmt.Sprintf("Promoting staging v%d to production (same images, production config)", rel.Version)))
	desc := fmt.Sprintf("Promote staging v%d", rel.Version)
	if rel.Description != "" {
		desc += ": " + rel.Description
	}
	return e.deployLocked(ctx, prod, DeploySpec{Compose: rel.Compose, Images: rel.Images, Description: desc, GitSHA: rel.GitSHA}, emit)
}

// UpdateConfig stores config changes and, if the stage is deployed, rolls
// out a new release with the same images.
func (e *Engine) UpdateConfig(ctx context.Context, app, stage string, req api.ConfigSetRequest, emit Emit) (int, error) {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	st, err := e.Store.GetStage(app, stage)
	if err != nil {
		return 0, err
	}
	if _, err := e.Store.UpdateConfig(st.ID, req.Set, req.Unset); err != nil {
		return 0, err
	}
	var changed []string
	for k := range req.Set {
		changed = append(changed, k)
	}
	sort.Strings(changed)
	desc := ""
	if len(changed) > 0 {
		desc = "Set " + strings.Join(changed, ", ")
	}
	if len(req.Unset) > 0 {
		if desc != "" {
			desc += "; "
		}
		desc += "Unset " + strings.Join(req.Unset, ", ")
	}
	emit(ev("config", "done", "Config updated: "+desc))
	if st.CurrentRelease == 0 || req.NoRestart {
		return 0, nil
	}
	st, _ = e.Store.GetStage(app, stage)
	cur, err := e.Store.GetRelease(st.ID, st.CurrentRelease)
	if err != nil {
		return 0, err
	}
	return e.deployLocked(ctx, st, DeploySpec{Compose: cur.Compose, Images: cur.Images, Description: desc, GitSHA: cur.GitSHA}, emit)
}

// Redeploy re-runs the current release (used after overrides change).
func (e *Engine) Redeploy(ctx context.Context, app, stage, desc string, emit Emit) (int, error) {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	st, err := e.Store.GetStage(app, stage)
	if err != nil {
		return 0, err
	}
	if st.CurrentRelease == 0 {
		return 0, nil
	}
	cur, err := e.Store.GetRelease(st.ID, st.CurrentRelease)
	if err != nil {
		return 0, err
	}
	return e.deployLocked(ctx, st, DeploySpec{Compose: cur.Compose, Images: cur.Images, Description: desc, GitSHA: cur.GitSHA}, emit)
}

// ---- lifecycle ----

func stoppedKey(stageID int64) string { return fmt.Sprintf("stopped/%d", stageID) }

func (e *Engine) Stopped(stageID int64) bool { return e.Store.Setting(stoppedKey(stageID), "") == "1" }

func (e *Engine) activeProjects(st store.Stage) []string {
	ps := []string{compose.DataProject(st.App, st.Name)}
	if st.ActiveColor != "" {
		ps = append(ps, compose.ColorProject(st.App, st.Name, st.ActiveColor))
	}
	return ps
}

// Stop stops all containers of a stage; they stay stopped across reboots.
func (e *Engine) Stop(ctx context.Context, app, stage string) error {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := e.Store.GetStage(app, stage)
	if err != nil {
		return err
	}
	if err := e.Store.SetSetting(stoppedKey(st.ID), "1"); err != nil {
		return err
	}
	_ = e.ApplyRoutes(ctx)
	for _, p := range e.activeProjects(st) {
		if cs, _ := e.projectContainers(ctx, p); len(cs) > 0 {
			if err := e.compose(ctx, p, "", nil, nil, "stop"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) Start(ctx context.Context, app, stage string) error {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := e.Store.GetStage(app, stage)
	if err != nil {
		return err
	}
	if err := e.Store.SetSetting(stoppedKey(st.ID), "0"); err != nil {
		return err
	}
	for _, p := range e.activeProjects(st) {
		if cs, _ := e.projectContainers(ctx, p); len(cs) > 0 {
			if err := e.compose(ctx, p, "", nil, nil, "start"); err != nil {
				return err
			}
		}
	}
	sleepCtx(ctx, time.Second)
	return e.ApplyRoutes(ctx)
}

// Restart restarts containers one by one (replicas keep serving).
func (e *Engine) Restart(ctx context.Context, app, stage, service string) (int, error) {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	st, err := e.Store.GetStage(app, stage)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range e.activeProjects(st) {
		cs, err := e.projectContainers(ctx, p)
		if err != nil {
			return n, err
		}
		for _, c := range cs {
			if service != "" && c.Labels[compose.LabelService] != service {
				continue
			}
			if c.Labels[compose.LabelOneShot] == "true" {
				continue
			}
			if err := e.Docker.Restart(ctx, c.ID); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// DestroyStage removes containers, volumes, network, images dir of a stage.
func (e *Engine) destroyStage(ctx context.Context, st store.Stage, emit Emit) {
	for _, p := range []string{compose.DataProject(st.App, st.Name), compose.ColorProject(st.App, st.Name, "blue"), compose.ColorProject(st.App, st.Name, "green")} {
		if cs, _ := e.projectContainers(ctx, p); len(cs) > 0 {
			emit(ev("destroy", "progress", "Removing "+p))
			if err := e.compose(ctx, p, "", nil, nil, "down", "--remove-orphans", "--timeout", "10"); err != nil {
				emit(ev("destroy", "warn", err.Error()))
			}
		}
	}
	vols, _ := e.Docker.Volumes(ctx, compose.LabelApp+"="+st.App)
	for _, v := range vols {
		if strings.HasPrefix(v, "dw-"+st.App+"-"+st.Name+"-") {
			emit(ev("destroy", "progress", "Removing volume "+v))
			if err := e.Docker.RemoveVolume(ctx, v); err != nil {
				emit(ev("destroy", "warn", err.Error()))
			}
		}
	}
	netName := compose.Network(st.App, st.Name)
	_ = e.Docker.DisconnectNetwork(ctx, netName, proxy.ContainerName)
	_ = e.Docker.RemoveNetwork(ctx, netName)
	_ = os.RemoveAll(filepath.Join(e.DataDir, "apps", st.App, st.Name))
	_ = e.Store.SetSetting(stoppedKey(st.ID), "0")
}

// DestroyApp removes everything belonging to an app, including its data.
func (e *Engine) DestroyApp(ctx context.Context, app string, emit Emit) error {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	a, err := e.Store.GetApp(app)
	if err != nil {
		return err
	}
	stages, err := e.Store.Stages(a.ID)
	if err != nil {
		return err
	}
	for _, st := range stages {
		e.destroyStage(ctx, st, emit)
	}
	if err := e.Store.DeleteApp(a.ID); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(e.DataDir, "apps", app))
	e.removeImages(ctx, app, nil)
	return e.ApplyRoutes(ctx)
}

// SetPipeline enables or disables the staging stage.
func (e *Engine) SetPipeline(ctx context.Context, app string, on bool, emit Emit) error {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	a, err := e.Store.GetApp(app)
	if err != nil {
		return err
	}
	if !on {
		if st, err := e.Store.GetStage(app, api.StagingStage); err == nil {
			e.destroyStage(ctx, st, emit)
		}
	}
	if err := e.Store.SetPipeline(a.ID, on); err != nil {
		return err
	}
	return e.ApplyRoutes(ctx)
}

// ---- images ----

func (e *Engine) pruneImages(ctx context.Context, app string) {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return
	}
	defer unlock()
	a, err := e.Store.GetApp(app)
	if err != nil {
		return
	}
	stages, _ := e.Store.Stages(a.ID)
	keep := map[string]bool{}
	for _, st := range stages {
		rels, _ := e.Store.Releases(st.ID, 100)
		kept := 0
		for _, r := range rels {
			if r.Version == st.CurrentRelease || ((r.Status == store.StatusSucceeded || r.Status == store.StatusSuperseded) && kept < e.KeepReleases) {
				for _, img := range r.Images {
					keep[img.Ref] = true
				}
				kept++
			}
		}
	}
	e.removeImages(ctx, app, keep)
}

func (e *Engine) removeImages(ctx context.Context, app string, keep map[string]bool) {
	imgs, err := e.Docker.Images(ctx, "dokwalt/"+app+"-*")
	if err != nil {
		return
	}
	for _, img := range imgs {
		for _, tag := range img.RepoTags {
			if !keep[tag] {
				if err := e.Docker.RemoveImage(ctx, tag); err == nil {
					e.Log.Info("pruned image", "image", tag)
				}
			}
		}
	}
}
