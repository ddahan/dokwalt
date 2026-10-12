package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/compose"
	"github.com/ddahan/dokwalt/internal/proxy"
	"github.com/ddahan/dokwalt/internal/store"
)

// RenameApp gives an app a new name, and every Docker resource named after
// it: projects, containers, network, volumes and images. Docker can't rename
// volumes, so each stage stops while its volumes are copied (the downtime),
// then starts again under the new name. The old volumes are removed only once
// the stage runs again; until the database is updated, any failure puts the
// app back as it was.
func (e *Engine) RenameApp(ctx context.Context, old, name string, emit Emit) error {
	unlock, err := e.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := compose.ValidName(name); err != nil {
		return err
	}
	if name == old {
		return fmt.Errorf("%s is already called %s", old, name)
	}
	a, err := e.Store.GetApp(old)
	if err != nil {
		return err
	}
	if _, err := e.Store.GetApp(name); err == nil {
		return fmt.Errorf("app %q already exists", name)
	}
	if users := e.Consumers(old); len(users) > 0 {
		return fmt.Errorf("%s is used by %s (x-dokwalt.uses), which reach it by its name — remove it from their compose files and redeploy them first", old, strings.Join(users, ", "))
	}
	stages, err := e.Store.Stages(a.ID)
	if err != nil {
		return err
	}

	// 1. Stop each stage and copy its volumes. Undone on failure.
	var moves []volumeMove
	downAt := map[string]time.Time{} // when each stage stopped, for its downtime
	undo := func() {
		emit(ev("rename", "warn", "Putting "+old+" back as it was"))
		for _, m := range moves {
			_ = e.Docker.RemoveVolume(ctx, m.to)
		}
		for _, st := range stages {
			_ = e.Docker.RemoveNetwork(ctx, compose.Network(name, st.Name))
			e.startStage(ctx, st)
		}
	}
	for _, st := range stages {
		if e.stopStage(ctx, st, emit) {
			downAt[st.Name] = time.Now()
		}
		ms, err := e.copyVolumes(ctx, st, name, emit)
		moves = append(moves, ms...)
		if err != nil {
			undo()
			return err
		}
	}

	// 2. The new name, in Docker and in the database.
	emit(ev("rename", "start", "Saving the new name"))
	retagged, err := e.retagImages(ctx, stages, old, name)
	if err != nil {
		for _, r := range retagged {
			_ = e.Docker.RemoveImage(ctx, r)
		}
		undo()
		return err
	}
	if err := e.Store.RenameApp(a.ID, old, name); err != nil {
		undo()
		return err
	}
	emit(ev("rename", "done", fmt.Sprintf("New name saved: releases, config, domains (%d images retagged)", len(retagged))))
	_ = os.RemoveAll(filepath.Join(e.DataDir, "apps", old))

	// 3. Start each stage under its new name.
	renamed, err := e.Store.Stages(a.ID)
	if err != nil {
		return err
	}
	healthy := true
	for _, st := range renamed {
		oldNet := compose.Network(old, st.Name)
		if st.CurrentRelease != 0 {
			emit(ev("rename", "start", "Starting "+st.Key()))
			switch err := e.startStage(ctx, st); {
			case err != nil:
				emit(ev("rename", "warn", st.Key()+": "+firstLine(err.Error())))
				healthy = false
			case e.Stopped(st.ID):
				emit(ev("rename", "done", "Created "+st.Key()+" (stays stopped)"))
			case !e.waitRunning(ctx, st):
				emit(ev("rename", "warn", st.Key()+" isn't running yet: check `dokwalt ps -a "+name+"`"))
				healthy = false
			default:
				msg := st.Key() + " is running"
				if t, ok := downAt[st.Name]; ok {
					msg += " — down for " + time.Since(t).Round(100*time.Millisecond).String()
				}
				emit(ev("rename", "done", msg))
			}
		}
		_ = e.Docker.DisconnectNetwork(ctx, oldNet, proxy.ContainerName)
		_ = e.Docker.RemoveNetwork(ctx, oldNet)
	}
	e.syncSharedNow(ctx)
	if err := e.ApplyRoutes(ctx); err != nil {
		return err
	}

	// 4. The old volumes, once the app runs from the copies.
	removed := 0
	for _, m := range moves {
		if !healthy {
			emit(ev("rename", "warn", "Kept the old volume "+m.from+": remove it once "+name+" works (docker volume rm "+m.from+")"))
			continue
		}
		if err := e.Docker.RemoveVolume(ctx, m.from); err != nil {
			emit(ev("rename", "warn", "Couldn't remove the old volume "+m.from+": "+err.Error()))
			continue
		}
		removed++
	}
	if removed > 0 {
		emit(ev("rename", "done", fmt.Sprintf("Removed the %d old volume(s)", removed)))
	}
	return nil
}

type volumeMove struct{ from, to string }

// AppVolumes lists the named volumes of each stage of an app: what a rename
// copies while the app is stopped.
func (e *Engine) AppVolumes(ctx context.Context, app string) ([]api.AppVolume, error) {
	a, err := e.Store.GetApp(app)
	if err != nil {
		return nil, err
	}
	stages, err := e.Store.Stages(a.ID)
	if err != nil {
		return nil, err
	}
	vols, err := e.Docker.Volumes(ctx, compose.LabelApp+"="+app)
	if err != nil {
		return nil, err
	}
	out := []api.AppVolume{}
	for _, st := range stages {
		for _, v := range vols {
			if name, ok := strings.CutPrefix(v, "dw-"+app+"-"+st.Name+"-"); ok {
				out = append(out, api.AppVolume{Stage: st.Name, Name: name, Volume: v})
			}
		}
	}
	return out, nil
}

// stopStage removes a stage's containers (data, blue and green projects),
// and reports whether it had any. Named volumes stay: they are external to
// the projects.
func (e *Engine) stopStage(ctx context.Context, st store.Stage, emit Emit) bool {
	stopped := false
	for _, p := range []string{compose.DataProject(st.App, st.Name), compose.ColorProject(st.App, st.Name, "blue"), compose.ColorProject(st.App, st.Name, "green")} {
		if cs, _ := e.projectContainers(ctx, p); len(cs) > 0 {
			if !stopped {
				emit(ev("rename", "start", "Stopping "+st.Key()))
				stopped = true
			}
			if err := e.compose(ctx, p, "", nil, nil, "down", "--remove-orphans", "--timeout", "20"); err != nil {
				emit(ev("rename", "warn", err.Error()))
			}
		}
	}
	if stopped {
		emit(ev("rename", "done", "Stopped "+st.Key()))
	}
	return stopped
}

// copyVolumes copies each volume of a stage to the volume of the same name
// under the new app name, with a throwaway container of the proxy's image.
func (e *Engine) copyVolumes(ctx context.Context, st store.Stage, name string, emit Emit) ([]volumeMove, error) {
	vols, err := e.Docker.Volumes(ctx, compose.LabelApp+"="+st.App)
	if err != nil {
		return nil, err
	}
	prefix := "dw-" + st.App + "-" + st.Name + "-"
	var moves []volumeMove
	for _, v := range vols {
		suffix, ok := strings.CutPrefix(v, prefix)
		if !ok {
			continue
		}
		to := compose.VolumeName(name, st.Name, suffix)
		emit(ev("rename", "start", "Copying volume "+v+" to "+to))
		if err := e.Docker.CreateVolume(ctx, to, map[string]string{compose.LabelApp: name, compose.LabelStage: st.Name}); err != nil {
			return moves, err
		}
		moves = append(moves, volumeMove{from: v, to: to})
		if out, err := e.docker(ctx, "run", "--rm", "--network", "none", "--entrypoint", "sh",
			"-v", v+":/from:ro", "-v", to+":/to", proxy.Image, "-c", "cp -a /from/. /to/"); err != nil {
			return moves, fmt.Errorf("copy volume %s: %w: %s", v, err, out)
		}
		emit(ev("rename", "done", "Copied volume "+v+" to "+to))
	}
	return moves, nil
}

// retagImages gives the app's images (those its releases use) the new name,
// so that deploys keep pruning them. It returns the new tags it created.
func (e *Engine) retagImages(ctx context.Context, stages []store.Stage, old, name string) ([]string, error) {
	from, to := "dokwalt/"+old+"-", "dokwalt/"+name+"-"
	seen := map[string]bool{}
	var created, untag []string
	for _, st := range stages {
		rels, err := e.Store.Releases(st.ID, -1) // all of them
		if err != nil {
			return created, err
		}
		for _, r := range rels {
			for _, img := range r.Images {
				rest, ok := strings.CutPrefix(img.Ref, from)
				if !ok || seen[img.Ref] {
					continue
				}
				seen[img.Ref] = true
				if ok, _ := e.Docker.ImageExists(ctx, img.Ref); !ok {
					continue // pruned already
				}
				if out, err := e.docker(ctx, "tag", img.Ref, to+rest); err != nil {
					return created, fmt.Errorf("tag %s: %w: %s", img.Ref, err, out)
				}
				created = append(created, to+rest)
				untag = append(untag, img.Ref)
			}
		}
	}
	for _, ref := range untag {
		_ = e.Docker.RemoveImage(ctx, ref) // only removes the old tag
	}
	return created, nil
}

// startStage brings a stage's projects up from its current release, as the
// reconciler does, with its network. A stopped stage gets its containers
// created then stopped, so that `dokwalt start` finds them.
func (e *Engine) startStage(ctx context.Context, st store.Stage) error {
	if st.CurrentRelease == 0 {
		return nil
	}
	netName := compose.Network(st.App, st.Name)
	if err := e.Docker.EnsureNetwork(ctx, netName, map[string]string{compose.LabelApp: st.App, compose.LabelStage: st.Name}); err != nil {
		return err
	}
	_ = e.Docker.ConnectNetwork(ctx, netName, proxy.ContainerName)
	if _, err := e.reconcileStage(ctx, st, nil); err != nil {
		return err
	}
	if e.Stopped(st.ID) {
		for _, p := range e.activeProjects(st) {
			_ = e.compose(ctx, p, "", nil, nil, "stop")
		}
	}
	return nil
}

// waitRunning waits up to two minutes for a stage's containers to run (and be
// healthy, for those with a health check).
func (e *Engine) waitRunning(ctx context.Context, st store.Stage) bool {
	for range 60 {
		if all, err := e.Docker.Containers(ctx, compose.LabelApp+"="+st.App); err == nil && e.StageStatus(ctx, st, all) == "running" {
			return true
		}
		if sleepCtx(ctx, 2*time.Second); ctx.Err() != nil {
			return false
		}
	}
	return false
}

// docker runs the docker CLI and returns its combined output.
func (e *Engine) docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.DockerBin, args...)
	cmd.Env = baseEnv()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
