package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/compose"
	"github.com/ddahan/dokwalt/internal/docker"
	"github.com/ddahan/dokwalt/internal/store"
)

// Shared services: an app lists other apps in `x-dokwalt.uses`, and the
// stateful containers of those apps (typically one Postgres for every site)
// join its private network under the provider's app name. Consumers still
// share no network with each other, so isolation between apps holds.

// ProviderStage picks the provider stage a consumer stage uses: the stage
// with the same name when the provider has one, else production.
func (e *Engine) ProviderStage(provider, stage string) (store.Stage, error) {
	st, err := e.Store.GetStage(provider, stage)
	if err == nil || stage == api.DefaultStage {
		return st, err
	}
	if _, aerr := e.Store.GetApp(provider); aerr != nil {
		return st, aerr
	}
	return e.Store.GetStage(provider, api.DefaultStage)
}

type sharedTarget struct {
	container string
	service   string
	aliases   []string
}

// providerTargets lists the running stateful containers of a provider stage,
// with the aliases they get on consumer networks.
func providerTargets(all []docker.ContainerSummary, pst store.Stage) []sharedTarget {
	var cs []docker.ContainerSummary
	services := map[string]bool{}
	for _, c := range all {
		l := c.Labels
		if l[compose.LabelApp] == pst.App && l[compose.LabelStage] == pst.Name && l[compose.LabelRole] == "data" && c.State == "running" {
			cs = append(cs, c)
			services[l[compose.LabelService]] = true
		}
	}
	out := make([]sharedTarget, 0, len(cs))
	for _, c := range cs {
		svc := c.Labels[compose.LabelService]
		out = append(out, sharedTarget{container: c.Name(), service: svc, aliases: compose.SharedAliases(pst.App, svc, len(services) == 1)})
	}
	return out
}

// checkUses fails a deploy early, before a release is created, when a
// provider is missing or not deployed.
func (e *Engine) checkUses(st store.Stage, uses []string) error {
	for _, p := range uses {
		pst, err := e.ProviderStage(p, st.Name)
		if err != nil {
			return fmt.Errorf("x-dokwalt.uses: %w", err)
		}
		if pst.CurrentRelease == 0 {
			return fmt.Errorf("x-dokwalt.uses: %s is not deployed yet — deploy it first", pst.Key())
		}
	}
	return nil
}

// linkShared attaches the providers' stateful containers to a consumer
// stage's network. It runs before the new color starts, so one-shot jobs
// (migrations) can already reach the shared database.
func (e *Engine) linkShared(ctx context.Context, st store.Stage, uses []string, emit Emit) error {
	if len(uses) == 0 {
		return nil
	}
	all, err := e.Docker.Containers(ctx, compose.LabelApp)
	if err != nil {
		return err
	}
	netName := compose.Network(st.App, st.Name)
	for _, p := range uses {
		pst, err := e.ProviderStage(p, st.Name)
		if err != nil {
			return fmt.Errorf("x-dokwalt.uses: %w", err)
		}
		targets := providerTargets(all, pst)
		if len(targets) == 0 {
			return fmt.Errorf("x-dokwalt.uses: %s has no running stateful service (is it stopped? `dokwalt ps -a %s`)", pst.Key(), pst.App)
		}
		var names []string
		for _, t := range targets {
			if err := e.Docker.ConnectNetwork(ctx, netName, t.container, t.aliases...); err != nil {
				return fmt.Errorf("connect %s to %s: %w", t.container, netName, err)
			}
			names = append(names, strings.Join(t.aliases, " / "))
		}
		emit(ev("data", "done", fmt.Sprintf("Shared %s reachable as %s", pst.Key(), strings.Join(names, ", "))))
	}
	return nil
}

// syncShared converges shared-service attachments: every provider container
// is on the networks of the stages that use it, under the right aliases, and
// on no other app network. A recreated provider container comes back without
// its extra networks; this puts them back.
func (e *Engine) syncShared(ctx context.Context, stages []store.Stage, all []docker.ContainerSummary) {
	desired := map[string]map[string][]string{} // container -> network -> aliases
	for _, st := range stages {
		for _, p := range e.stageUses(st) {
			pst, err := e.ProviderStage(p, st.Name)
			if err != nil || pst.CurrentRelease == 0 {
				continue
			}
			for _, t := range providerTargets(all, pst) {
				if desired[t.container] == nil {
					desired[t.container] = map[string][]string{}
				}
				desired[t.container][compose.Network(st.App, st.Name)] = t.aliases
			}
		}
	}
	for _, c := range all {
		if c.Labels[compose.LabelRole] != "data" || c.State != "running" {
			continue
		}
		ci, err := e.Docker.Inspect(ctx, c.ID)
		if err != nil {
			continue
		}
		want := desired[c.Name()]
		for netName, aliases := range want {
			have, ok := ci.NetworkSettings.Networks[netName]
			if ok && containsAll(have.Aliases, aliases) {
				continue
			}
			if ok {
				_ = e.Docker.DisconnectNetwork(ctx, netName, c.ID)
			}
			if err := e.Docker.ConnectNetwork(ctx, netName, c.ID, aliases...); err != nil {
				e.Log.Error("link shared service", "container", c.Name(), "network", netName, "err", err)
				continue
			}
			e.Log.Info("linked shared service", "container", c.Name(), "network", netName)
		}
		own := compose.Network(c.Labels[compose.LabelApp], c.Labels[compose.LabelStage])
		for netName := range ci.NetworkSettings.Networks {
			if _, ok := want[netName]; ok || netName == own || !strings.HasPrefix(netName, "dw-") {
				continue
			}
			if err := e.Docker.DisconnectNetwork(ctx, netName, c.ID); err == nil {
				e.Log.Info("unlinked shared service", "container", c.Name(), "network", netName)
			}
		}
	}
}

// syncSharedNow runs syncShared with fresh state (after a rollout, which may
// have recreated a provider's containers).
func (e *Engine) syncSharedNow(ctx context.Context) {
	stages, err := e.Store.Stages(0)
	if err != nil {
		return
	}
	all, err := e.Docker.Containers(ctx, compose.LabelApp)
	if err != nil {
		return
	}
	e.syncShared(ctx, stages, all)
}

// stageUses returns the providers listed by a stage's current release.
func (e *Engine) stageUses(st store.Stage) []string {
	if st.CurrentRelease == 0 {
		return nil
	}
	rel, err := e.Store.GetRelease(st.ID, st.CurrentRelease)
	if err != nil {
		return nil
	}
	uses, _, _ := compose.Uses(rel.Compose)
	return uses
}

// Consumers lists the stages of other apps that use app.
func (e *Engine) Consumers(app string) []string {
	stages, err := e.Store.Stages(0)
	if err != nil {
		return nil
	}
	var out []string
	for _, st := range stages {
		if st.App != app && slices.Contains(e.stageUses(st), app) {
			out = append(out, st.Key())
		}
	}
	return out
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}
