package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/compose"
	"github.com/ddahan/dokwalt/internal/docker"
	"github.com/ddahan/dokwalt/internal/proxy"
	"github.com/ddahan/dokwalt/internal/store"
)

// WaitForDocker blocks until the Docker API answers (after a reboot dockerd
// may still be starting even though systemd ordered us after it).
func (e *Engine) WaitForDocker(ctx context.Context) error {
	delay := 500 * time.Millisecond
	for {
		if err := e.Docker.Ping(ctx); err == nil {
			_, err := e.Docker.Negotiate(ctx)
			return err
		}
		e.Log.Info("waiting for docker")
		sleepCtx(ctx, delay)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if delay < 10*time.Second {
			delay *= 2
		}
	}
}

// Recover handles deploys interrupted by a crash or reboot. Call once at startup.
func (e *Engine) Recover(ctx context.Context) {
	rels, err := e.Store.ReleasesWithStatus(store.StatusDeploying)
	if err != nil {
		e.Log.Error("recover", "err", err)
		return
	}
	for _, r := range rels {
		st, err := e.Store.GetStageByID(r.StageID)
		if err != nil {
			continue
		}
		e.Log.Warn("release interrupted, marking failed", "stage", st.Key(), "version", r.Version)
		_ = e.Store.FinishRelease(r.ID, store.StatusFailed, "interrupted (daemon restart or reboot during deploy)")
		if r.Color != "" && r.Color != st.ActiveColor {
			_ = e.compose(ctx, compose.ColorProject(st.App, st.Name, r.Color), "", nil, nil, "down", "--remove-orphans", "--timeout", "5")
		}
	}
}

// Reconcile converges actual state to desired state. It is cheap when
// nothing is wrong: a few Docker API calls, no compose invocations.
func (e *Engine) Reconcile(ctx context.Context) error {
	unlock, ok := e.tryAcquire()
	if !ok {
		return nil // a deploy is running; it owns the state right now
	}
	defer unlock()

	if err := e.ensureProxy(ctx); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	stages, err := e.Store.Stages(0)
	if err != nil {
		return err
	}
	all, err := e.Docker.Containers(ctx, compose.LabelApp)
	if err != nil {
		return err
	}
	proxyNets, _ := e.proxyNetworks(ctx)
	changed := false
	for _, st := range stages {
		if st.CurrentRelease == 0 {
			continue
		}
		netName := compose.Network(st.App, st.Name)
		if err := e.Docker.EnsureNetwork(ctx, netName, map[string]string{compose.LabelApp: st.App, compose.LabelStage: st.Name}); err != nil {
			return err
		}
		if !proxyNets[netName] {
			if err := e.Docker.ConnectNetwork(ctx, netName, proxy.ContainerName); err == nil {
				changed = true
			}
		}
		if e.Stopped(st.ID) {
			continue
		}
		fixed, err := e.reconcileStage(ctx, st, all)
		if err != nil {
			e.Log.Error("reconcile stage", "stage", st.Key(), "err", err)
			e.Alert("reconcile:"+st.Key(), "Self-healing failed", fmt.Sprintf("%s: %s", st.Key(), firstLine(err.Error())), true)
			continue
		}
		e.Alert("reconcile:"+st.Key(), "Self-healing", st.Key()+" recovered", false)
		changed = changed || fixed
	}
	e.cleanupStale(ctx, stages, all)
	if changed {
		// Containers were just (re)started: shared links need their current names.
		if fresh, err := e.Docker.Containers(ctx, compose.LabelApp); err == nil {
			all = fresh
		}
	}
	e.syncShared(ctx, stages, all)

	// Caddy must run the config on disk, with fresh upstreams.
	routes, err := e.Routes(ctx, nil)
	if err != nil {
		return err
	}
	desired := proxy.HashConfig(e.Proxy.Config(routes))
	if changed || desired != e.Proxy.FileHash() || desired != e.Proxy.LoadedHash(ctx) {
		e.Log.Info("reloading proxy config")
		if err := e.Proxy.Apply(ctx, routes); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) proxyNetworks(ctx context.Context) (map[string]bool, error) {
	ci, err := e.Docker.Inspect(ctx, proxy.ContainerName)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for n := range ci.NetworkSettings.Networks {
		out[n] = true
	}
	return out, nil
}

// reconcileStage makes sure the data and active color projects run.
func (e *Engine) reconcileStage(ctx context.Context, st store.Stage, all []docker.ContainerSummary) (bool, error) {
	rel, err := e.Store.GetRelease(st.ID, st.CurrentRelease)
	if err != nil {
		return false, err
	}
	plan, cfg, err := e.plan(st, DeploySpec{Compose: rel.Compose, Images: rel.Images}, rel.Version)
	if err != nil {
		return false, err
	}
	type want struct {
		project string
		doc     map[string]any
	}
	var wants []want
	if plan.Data != nil {
		wants = append(wants, want{compose.DataProject(st.App, st.Name), plan.Data})
	}
	if plan.App != nil && st.ActiveColor != "" {
		doc, err := compose.RenderApp(plan, st.App, st.Name, st.ActiveColor)
		if err != nil {
			return false, err
		}
		wants = append(wants, want{compose.ColorProject(st.App, st.Name, st.ActiveColor), doc})
	}
	fixed := false
	for _, w := range wants {
		services := w.doc["services"].(map[string]any)
		missing := []string{}
		for name, s := range services {
			if labels, ok := s.(map[string]any)["labels"].(map[string]string); ok && labels[compose.LabelOneShot] == "true" {
				continue
			}
			if !hasRunning(all, w.project, name) {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			continue
		}
		e.Log.Warn("reconciling", "project", w.project, "missing", missing)
		for _, v := range plan.Volumes {
			_ = e.Docker.CreateVolume(ctx, v, map[string]string{compose.LabelApp: st.App, compose.LabelStage: st.Name})
		}
		file, err := e.composeFile(st.App, st.Name, w.project, w.doc)
		if err != nil {
			return fixed, err
		}
		if err := e.compose(ctx, w.project, file, cfg, nil, "up", "-d", "--remove-orphans"); err != nil {
			return fixed, err
		}
		fixed = true
	}
	return fixed, nil
}

func hasRunning(all []docker.ContainerSummary, project, service string) bool {
	for _, c := range all {
		if c.Labels["com.docker.compose.project"] == project && c.Labels["com.docker.compose.service"] == service && (c.State == "running" || c.State == "restarting") {
			return true
		}
	}
	return false
}

// cleanupStale removes inactive color projects left behind (e.g. a drain
// interrupted by a reboot) and containers of apps that no longer exist.
func (e *Engine) cleanupStale(ctx context.Context, stages []store.Stage, all []docker.ContainerSummary) {
	active := map[string]bool{}
	known := map[string]bool{}
	for _, st := range stages {
		known[st.Key()] = true
		active[compose.DataProject(st.App, st.Name)] = true
		if st.ActiveColor != "" {
			active[compose.ColorProject(st.App, st.Name, st.ActiveColor)] = true
		}
	}
	stale := map[string]bool{}
	for _, c := range all {
		p := c.Labels["com.docker.compose.project"]
		key := c.Labels[compose.LabelApp] + "/" + c.Labels[compose.LabelStage]
		if p == "" || active[p] || e.isDraining(p) {
			continue
		}
		// Only clean color projects of known stages that are older than the drain delay.
		if known[key] && c.Labels[compose.LabelColor] != "" {
			stale[p] = true
		}
	}
	for p := range stale {
		e.Log.Info("removing inactive project", "project", p)
		_ = e.compose(ctx, p, "", nil, nil, "down", "--remove-orphans", "--timeout", "20")
	}
}

// StageStatus summarizes a stage for listings.
func (e *Engine) StageStatus(ctx context.Context, st store.Stage, all []docker.ContainerSummary) string {
	if e.IsDeploying(st.ID) {
		return "deploying"
	}
	if st.CurrentRelease == 0 {
		return "not deployed"
	}
	if e.Stopped(st.ID) {
		return "stopped"
	}
	running, total := 0, 0
	for _, c := range all {
		l := c.Labels
		if l[compose.LabelApp] != st.App || l[compose.LabelStage] != st.Name || l[compose.LabelOneShot] == "true" {
			continue
		}
		if l[compose.LabelRole] == "app" && l[compose.LabelColor] != st.ActiveColor {
			continue
		}
		total++
		if c.State == "running" && !strings.Contains(c.Status, "unhealthy") {
			running++
		}
	}
	switch {
	case total == 0:
		return "down"
	case running == total:
		return "running"
	case running == 0:
		return "down"
	default:
		return "degraded"
	}
}

// Containers returns API views of a stage's active containers.
func (e *Engine) StageContainers(ctx context.Context, st store.Stage) (map[string][]api.Container, error) {
	cs, err := e.Docker.Containers(ctx, compose.LabelApp+"="+st.App, compose.LabelStage+"="+st.Name)
	if err != nil {
		return nil, err
	}
	out := map[string][]api.Container{}
	netName := compose.Network(st.App, st.Name)
	for _, c := range cs {
		l := c.Labels
		if l[compose.LabelRole] == "app" && l[compose.LabelColor] != st.ActiveColor {
			continue
		}
		ci, err := e.Docker.Inspect(ctx, c.ID)
		if err != nil {
			continue
		}
		out[l[compose.LabelService]] = append(out[l[compose.LabelService]], api.Container{
			ID: c.ID[:12], Name: c.Name(), State: c.State, Status: c.Status, Health: ci.Health(),
			Restarts: ci.RestartCount, Color: l[compose.LabelColor], IP: ci.NetworkSettings.Networks[netName].IPAddress,
		})
	}
	return out, nil
}
