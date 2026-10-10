package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/compose"
	"github.com/ddahan/dokwalt/internal/docker"
	"github.com/ddahan/dokwalt/internal/engine"
	"github.com/ddahan/dokwalt/internal/metrics"
	"github.com/ddahan/dokwalt/internal/store"
)

func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r); err != nil {
				writeErr(w, err)
			}
		})
	}
	h("GET /v1/version", d.getVersion)
	h("GET /v1/info", d.getInfo)
	h("GET /v1/settings", d.getSettings)
	h("POST /v1/settings", d.postSettings)
	h("GET /v1/doctor", d.getDoctor)
	h("GET /v1/top", d.getTop)
	h("GET /v1/metrics", d.getMetrics)
	h("POST /v1/images/have", d.postImagesHave)
	h("POST /v1/images/load", d.postImagesLoad)

	h("GET /v1/backups/config", d.getBackupConfig)
	h("POST /v1/backups/config", d.setBackupConfig)
	h("DELETE /v1/backups/config", d.deleteBackupConfig)
	h("POST /v1/backups/run", d.postBackupRun)
	h("GET /v1/backups", d.listBackups)
	h("GET /v1/backups/{id}", d.getBackup)
	h("GET /v1/backups/{id}/files/{path...}", d.getBackupFile)
	h("POST /v1/backups/{id}/restore", d.postBackupRestore)

	h("GET /v1/apps", d.listApps)
	h("POST /v1/apps", d.createApp)
	h("POST /v1/apps/import", d.importApp)
	h("GET /v1/apps/{app}", d.getApp)
	h("DELETE /v1/apps/{app}", d.destroyApp)
	h("GET /v1/apps/{app}/export", d.exportApp)
	h("POST /v1/apps/{app}/pipeline", d.setPipeline)
	h("POST /v1/apps/{app}/promote", d.promote)

	s := "/v1/apps/{app}/stages/{stage}"
	h("GET "+s+"/services", d.listServices)
	h("POST "+s+"/services/{service}", d.setOverride)
	h("POST "+s+"/deploy", d.deploy)
	h("GET "+s+"/releases", d.listReleases)
	h("POST "+s+"/rollback", d.rollback)
	h("GET "+s+"/config", d.getConfig)
	h("POST "+s+"/config", d.setConfig)
	h("GET "+s+"/domains", d.listDomains)
	h("POST "+s+"/domains", d.addDomain)
	h("DELETE "+s+"/domains/{host}", d.removeDomain)
	h("POST "+s+"/restart", d.restart)
	h("POST "+s+"/stop", d.stopStage)
	h("POST "+s+"/start", d.startStage)
	h("GET "+s+"/logs", d.logs)
	h("GET "+s+"/db", d.dbTarget)
	h("GET "+s+"/exec", d.execTarget)
	h("GET "+s+"/metrics", d.stageMetrics)

	h("GET /v1/alerts", d.getAlerts)
	h("POST /v1/alerts/channels", d.addAlertChannel)
	h("DELETE /v1/alerts/channels/{id}", d.removeAlertChannel)
	h("POST /v1/alerts/test", d.testAlerts)
	h("POST /v1/alerts/settings", d.setAlertSettings)
	return mux
}

// ---- helpers ----

type httpError struct {
	status int
	msg    string
	hint   string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return &httpError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, a...)}
}

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	body := api.Error{Error: err.Error()}
	var he *httpError
	switch {
	case errors.As(err, &he):
		status, body.Hint = he.status, he.hint
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

// streamer writes NDJSON events; the last event is "result" or "error".
type streamer struct {
	mu  sync.Mutex
	w   http.ResponseWriter
	enc *json.Encoder
	fl  http.Flusher
}

func newStreamer(w http.ResponseWriter) *streamer {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	return &streamer{w: w, enc: json.NewEncoder(w), fl: fl}
}

func (s *streamer) emit(e api.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	_ = s.enc.Encode(e)
	if s.fl != nil {
		s.fl.Flush()
	}
}

func (s *streamer) finish(err error, result string, release int) error {
	if err != nil {
		s.emit(api.Event{Status: "error", Message: err.Error(), Release: release})
	} else {
		s.emit(api.Event{Status: "result", Message: result, Release: release})
	}
	return nil
}

func (d *Daemon) stageOf(r *http.Request) (store.Stage, error) {
	return d.store.GetStage(r.PathValue("app"), r.PathValue("stage"))
}

// opCtx detaches long operations from the client connection: a laptop going
// to sleep mid-deploy must not leave a half-finished rollout.
func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Minute)
}

// ---- server ----

func (d *Daemon) getVersion(w http.ResponseWriter, r *http.Request) error {
	return writeJSON(w, api.VersionInfo{APIVersion: api.Version, Build: d.opts.Build, OS: runtime.GOOS, Arch: runtime.GOARCH})
}

func (d *Daemon) getInfo(w http.ResponseWriter, r *http.Request) error {
	info := api.ServerInfo{
		Version:   api.VersionInfo{APIVersion: api.Version, Build: d.opts.Build, OS: runtime.GOOS, Arch: runtime.GOARCH},
		Host:      d.collector.Host(),
		ACMEEmail: d.store.Setting("acme_email", ""),
		Backup:    d.backupStatus(),
	}
	info.DaemonRSS, info.DaemonHeap = metrics.ProcessMem(os.Getpid())
	info.Hostname, _ = os.Hostname()
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		f, _ := strconv.ParseFloat(strings.Fields(string(b))[0], 64)
		info.Uptime = int64(f)
	}
	if v, err := d.docker.Negotiate(r.Context()); err == nil {
		info.DockerVersion = v.Version
	}
	if out, err := exec.CommandContext(r.Context(), "docker", "compose", "version", "--short").Output(); err == nil {
		info.ComposeVer = strings.TrimSpace(string(out))
	}
	if ci, err := d.docker.Inspect(r.Context(), "dokwalt-caddy"); err == nil {
		info.CaddyRSS, info.CaddyHeap = metrics.ProcessMem(ci.State.Pid)
	}
	apps, _ := d.store.ListApps()
	info.Apps = len(apps)
	return writeJSON(w, info)
}

// settingKeys lists server settings and their defaults.
var settingKeys = map[string]string{"acme_email": "", "keep_releases": "5", "drain": "10s", "metrics_retention_days": "7"}

func (d *Daemon) getSettings(w http.ResponseWriter, r *http.Request) error {
	out := map[string]string{}
	for k, def := range settingKeys {
		out[k] = d.store.Setting(k, def)
	}
	return writeJSON(w, out)
}

func (d *Daemon) postSettings(w http.ResponseWriter, r *http.Request) error {
	var in map[string]string
	if err := readJSON(r, &in); err != nil {
		return err
	}
	for k, v := range in {
		if _, ok := settingKeys[k]; !ok {
			return badRequest("unknown setting %q (known: acme_email, keep_releases, drain, metrics_retention_days)", k)
		}
		switch k {
		case "keep_releases", "metrics_retention_days":
			if n, err := strconv.Atoi(v); err != nil || n < 1 {
				return badRequest("%s must be a positive integer", k)
			}
		case "drain":
			if _, err := time.ParseDuration(v); err != nil {
				return badRequest("drain must be a duration like 10s")
			}
		}
		if err := d.store.SetSetting(k, v); err != nil {
			return err
		}
		switch k {
		case "acme_email":
			d.proxy.Email = v
		case "keep_releases":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				d.engine.KeepReleases = n
			}
		case "drain":
			if dur, err := time.ParseDuration(v); err == nil {
				d.engine.Drain = dur
			}
		case "metrics_retention_days":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				d.collector.Retention = time.Duration(n) * 24 * time.Hour
			}
		}
	}
	if _, ok := in["acme_email"]; ok {
		_ = d.engine.ApplyRoutes(r.Context())
	}
	return writeJSON(w, map[string]string{"status": "ok"})
}

// ---- images ----

func (d *Daemon) postImagesHave(w http.ResponseWriter, r *http.Request) error {
	var in api.ImagesHaveRequest
	if err := readJSON(r, &in); err != nil {
		return err
	}
	out := api.ImagesHaveResponse{Have: map[string]bool{}}
	for _, id := range in.IDs {
		ok, err := d.docker.ImageExists(r.Context(), id)
		if err != nil {
			return err
		}
		out.Have[id] = ok
	}
	return writeJSON(w, out)
}

// postImagesLoad receives a zstd-compressed `docker save` stream.
func (d *Daemon) postImagesLoad(w http.ResponseWriter, r *http.Request) error {
	dec, err := zstd.NewReader(r.Body, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return err
	}
	defer dec.Close()
	if err := d.docker.LoadImage(r.Context(), dec); err != nil {
		return fmt.Errorf("load image: %w", err)
	}
	return writeJSON(w, map[string]string{"status": "loaded"})
}

// ---- apps ----

func (d *Daemon) appView(ctx context.Context, a store.App, all []docker.ContainerSummary) api.App {
	out := api.App{Name: a.Name, Pipeline: a.Pipeline, CreatedAt: a.CreatedAt}
	stages, _ := d.store.Stages(a.ID)
	for _, st := range stages {
		v := api.Stage{Name: st.Name, ActiveColor: st.ActiveColor, CurrentRelease: st.CurrentRelease, Status: d.engine.StageStatus(ctx, st, all)}
		doms, _ := d.store.Domains(st.ID)
		for _, dm := range doms {
			v.Domains = append(v.Domains, dm.Hostname)
		}
		out.Stages = append(out.Stages, v)
	}
	return out
}

func (d *Daemon) listApps(w http.ResponseWriter, r *http.Request) error {
	apps, err := d.store.ListApps()
	if err != nil {
		return err
	}
	all, err := d.docker.Containers(r.Context(), compose.LabelApp)
	if err != nil {
		return err
	}
	out := []api.App{}
	for _, a := range apps {
		out = append(out, d.appView(r.Context(), a, all))
	}
	return writeJSON(w, out)
}

func (d *Daemon) createApp(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := compose.ValidName(in.Name); err != nil {
		return badRequest("%v", err)
	}
	a, err := d.store.CreateApp(in.Name)
	if err != nil {
		return &httpError{status: http.StatusConflict, msg: err.Error()}
	}
	return writeJSON(w, d.appView(r.Context(), a, nil))
}

func (d *Daemon) getApp(w http.ResponseWriter, r *http.Request) error {
	a, err := d.store.GetApp(r.PathValue("app"))
	if err != nil {
		return err
	}
	all, err := d.docker.Containers(r.Context(), compose.LabelApp+"="+a.Name)
	if err != nil {
		return err
	}
	return writeJSON(w, d.appView(r.Context(), a, all))
}

func (d *Daemon) destroyApp(w http.ResponseWriter, r *http.Request) error {
	if _, err := d.store.GetApp(r.PathValue("app")); err != nil {
		return err
	}
	s := newStreamer(w)
	ctx, cancel := opCtx()
	defer cancel()
	err := d.engine.DestroyApp(ctx, r.PathValue("app"), s.emit)
	d.refreshHosts()
	return s.finish(err, "destroyed", 0)
}

func (d *Daemon) setPipeline(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	a, err := d.store.GetApp(r.PathValue("app"))
	if err != nil {
		return err
	}
	if a.Pipeline == in.Enabled {
		return writeJSON(w, map[string]bool{"enabled": in.Enabled})
	}
	s := newStreamer(w)
	ctx, cancel := opCtx()
	defer cancel()
	err = d.engine.SetPipeline(ctx, a.Name, in.Enabled, s.emit)
	d.refreshHosts()
	return s.finish(err, "ok", 0)
}

func (d *Daemon) promote(w http.ResponseWriter, r *http.Request) error {
	a, err := d.store.GetApp(r.PathValue("app"))
	if err != nil {
		return err
	}
	if !a.Pipeline {
		return &httpError{status: http.StatusBadRequest, msg: a.Name + " has no pipeline", hint: "enable it with `dokwalt pipeline:enable`"}
	}
	s := newStreamer(w)
	ctx, cancel := opCtx()
	defer cancel()
	v, err := d.engine.Promote(ctx, a.Name, s.emit)
	return s.finish(err, "promoted", v)
}

func (d *Daemon) exportApp(w http.ResponseWriter, r *http.Request) error {
	a, err := d.store.GetApp(r.PathValue("app"))
	if err != nil {
		return err
	}
	out := api.AppExport{Format: 1, Name: a.Name, Pipeline: a.Pipeline, Stages: map[string]api.StageExport{}}
	stages, _ := d.store.Stages(a.ID)
	for _, st := range stages {
		cfg, err := d.store.Config(st.ID)
		if err != nil {
			return err
		}
		se := api.StageExport{Config: cfg}
		doms, _ := d.store.Domains(st.ID)
		for _, dm := range doms {
			se.Domains = append(se.Domains, api.Domain{Hostname: dm.Hostname, Service: dm.Service, Port: dm.Port, RedirectTo: dm.RedirectTo})
		}
		ovs, _ := d.store.Overrides(st.ID)
		for _, o := range ovs {
			se.Overrides = append(se.Overrides, api.ServiceOverride{Service: o.Service, Stateful: o.Stateful, HealthPath: o.HealthPath, HealthTimeout: o.HealthTimeout})
		}
		out.Stages[st.Name] = se
	}
	return writeJSON(w, out)
}

func (d *Daemon) importApp(w http.ResponseWriter, r *http.Request) error {
	var in api.AppExport
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.Format != 1 {
		return badRequest("unsupported export format %d", in.Format)
	}
	if err := compose.ValidName(in.Name); err != nil {
		return badRequest("%v", err)
	}
	// Check conflicts first so a failed import leaves nothing behind.
	existing, err := d.store.Domains(0)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, dm := range existing {
		taken[dm.Hostname] = true
	}
	for _, se := range in.Stages {
		for _, dm := range se.Domains {
			if taken[dm.Hostname] {
				return &httpError{status: http.StatusConflict, msg: "domain " + dm.Hostname + " is already attached to another app", hint: "remove it there first, or edit the export file"}
			}
		}
	}
	a, err := d.store.CreateApp(in.Name)
	if err != nil {
		return &httpError{status: http.StatusConflict, msg: err.Error(), hint: "destroy it first or import under another name"}
	}
	ok := false
	defer func() {
		if !ok {
			_ = d.store.DeleteApp(a.ID)
		}
	}()
	if in.Pipeline {
		if err := d.store.SetPipeline(a.ID, true); err != nil {
			return err
		}
	}
	for name, se := range in.Stages {
		st, err := d.store.GetStage(a.Name, name)
		if err != nil {
			return err
		}
		if len(se.Config) > 0 {
			if _, err := d.store.UpdateConfig(st.ID, se.Config, nil); err != nil {
				return err
			}
		}
		for _, dm := range se.Domains {
			if err := d.store.AddDomain(store.Domain{StageID: st.ID, Hostname: dm.Hostname, Service: dm.Service, Port: dm.Port, RedirectTo: dm.RedirectTo}); err != nil {
				return err
			}
		}
		for _, o := range se.Overrides {
			if err := d.store.SetOverride(st.ID, store.Override{Service: o.Service, Stateful: o.Stateful, HealthPath: o.HealthPath, HealthTimeout: o.HealthTimeout}); err != nil {
				return err
			}
		}
	}
	ok = true
	d.refreshHosts()
	_ = d.engine.ApplyRoutes(r.Context())
	return writeJSON(w, d.appView(r.Context(), a, nil))
}

// ---- services ----

func (d *Daemon) currentPlan(st store.Stage) (*compose.Plan, error) {
	if st.CurrentRelease == 0 {
		return nil, nil
	}
	rel, err := d.store.GetRelease(st.ID, st.CurrentRelease)
	if err != nil {
		return nil, err
	}
	cfg, _ := d.store.Config(st.ID)
	ovs, _ := d.store.Overrides(st.ID)
	stateful := map[string]*bool{}
	for k, o := range ovs {
		stateful[k] = o.Stateful
	}
	refs := map[string]string{}
	for k, img := range rel.Images {
		refs[k] = img.Ref
	}
	plan, err := compose.Transform(compose.Input{App: st.App, Stage: st.Name, Compose: rel.Compose, Images: refs, Stateful: stateful, ConfigKeys: store.SortedKeys(cfg)})
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

func (d *Daemon) listServices(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	plan, err := d.currentPlan(st)
	if err != nil {
		return err
	}
	out := []api.Service{}
	if plan == nil {
		return writeJSON(w, out)
	}
	containers, _ := d.engine.StageContainers(r.Context(), st)
	ovs, _ := d.store.Overrides(st.ID)
	for _, s := range plan.Services {
		out = append(out, api.Service{
			Name: s.Name, Image: s.Image, Stateful: s.Stateful, Detected: s.Reason, Overridden: s.Overridden,
			HealthPath: ovs[s.Name].HealthPath, Containers: containers[s.Name],
			Metrics: d.collector.Service(st.App, st.Name, s.Name),
		})
	}
	return writeJSON(w, out)
}

func (d *Daemon) setOverride(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	var in api.ServiceOverride
	if err := readJSON(r, &in); err != nil {
		return err
	}
	svc := r.PathValue("service")
	ovs, err := d.store.Overrides(st.ID)
	if err != nil {
		return err
	}
	o := ovs[svc]
	o.Service = svc
	redeploy := false
	if in.Stateful != nil {
		o.Stateful = in.Stateful
		redeploy = true
	}
	if in.ClearHealth {
		o.HealthPath, o.HealthTimeout = "", 0
	}
	if in.HealthPath != "" {
		if !strings.HasPrefix(in.HealthPath, "/") {
			return badRequest("health path must start with /")
		}
		o.HealthPath = in.HealthPath
	}
	if in.HealthTimeout > 0 {
		o.HealthTimeout = in.HealthTimeout
	}
	if err := d.store.SetOverride(st.ID, o); err != nil {
		return err
	}
	if !redeploy || st.CurrentRelease == 0 {
		return writeJSON(w, map[string]string{"status": "saved"})
	}
	s := newStreamer(w)
	ctx, cancel := opCtx()
	defer cancel()
	v, err := d.engine.Redeploy(ctx, st.App, st.Name, "Service "+svc+" stateful="+strconv.FormatBool(*in.Stateful), s.emit)
	return s.finish(err, "redeployed", v)
}

// ---- deploy & releases ----

func (d *Daemon) deploy(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	var in struct {
		api.DeployRequest
		ProjectDir string `json:"project_dir"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	s := newStreamer(w)
	ctx, cancel := opCtx()
	defer cancel()
	v, err := d.engine.Deploy(ctx, st.App, st.Name, engine.DeploySpec{
		Compose: in.Compose, Images: in.Images, Description: in.Description, GitSHA: in.GitSHA, ProjectDir: in.ProjectDir,
	}, s.emit)
	return s.finish(err, "deployed", v)
}

func (d *Daemon) listReleases(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 20
	}
	rels, err := d.store.Releases(st.ID, limit)
	if err != nil {
		return err
	}
	out := []api.Release{}
	for _, rel := range rels {
		out = append(out, api.Release{
			Version: rel.Version, Status: rel.Status, Images: rel.Images, ConfigVersion: rel.ConfigVersion,
			Color: rel.Color, Description: rel.Description, GitSHA: rel.GitSHA, CreatedAt: rel.CreatedAt,
			FinishedAt: rel.FinishedAt, Error: rel.Error, Current: rel.Version == st.CurrentRelease,
		})
	}
	return writeJSON(w, out)
}

func (d *Daemon) rollback(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	var in api.RollbackRequest
	if err := readJSON(r, &in); err != nil {
		return err
	}
	s := newStreamer(w)
	ctx, cancel := opCtx()
	defer cancel()
	v, err := d.engine.Rollback(ctx, st.App, st.Name, in.Version, in.WithConfig, s.emit)
	return s.finish(err, "rolled back", v)
}

// ---- config ----

func (d *Daemon) getConfig(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	cfg, err := d.store.Config(st.ID)
	if err != nil {
		return err
	}
	reveal := r.URL.Query().Get("reveal") == "1"
	out := []api.ConfigVar{}
	for _, k := range store.SortedKeys(cfg) {
		v := cfg[k]
		if !reveal {
			v = mask(v)
		}
		out = append(out, api.ConfigVar{Key: k, Value: v})
	}
	return writeJSON(w, out)
}

func mask(v string) string {
	if len(v) <= 4 {
		return strings.Repeat("•", len(v))
	}
	return v[:2] + strings.Repeat("•", min(len(v)-2, 12))
}

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (d *Daemon) setConfig(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	var in api.ConfigSetRequest
	if err := readJSON(r, &in); err != nil {
		return err
	}
	for k := range in.Set {
		if !keyRe.MatchString(k) {
			return badRequest("invalid config key %q (letters, digits and underscores)", k)
		}
	}
	if len(in.Set) == 0 && len(in.Unset) == 0 {
		return badRequest("nothing to change")
	}
	s := newStreamer(w)
	ctx, cancel := opCtx()
	defer cancel()
	v, err := d.engine.UpdateConfig(ctx, st.App, st.Name, in, s.emit)
	return s.finish(err, "config updated", v)
}

// ---- domains ----

var hostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$|^localhost$`)

func (d *Daemon) listDomains(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	doms, err := d.store.Domains(st.ID)
	if err != nil {
		return err
	}
	check := r.URL.Query().Get("check") == "1"
	out := make([]api.Domain, len(doms))
	var wg sync.WaitGroup
	plan, _ := d.currentPlan(st)
	for i, dm := range doms {
		out[i] = api.Domain{Hostname: dm.Hostname, Stage: st.Name, Service: dm.Service, Port: d.domainPort(plan, dm), RedirectTo: dm.RedirectTo}
		if check {
			wg.Add(1)
			go func(i int, host string) {
				defer wg.Done()
				out[i].Check = d.checkDomain(r.Context(), host)
			}(i, dm.Hostname)
		}
	}
	wg.Wait()
	return writeJSON(w, out)
}

func (d *Daemon) addDomain(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	var in api.DomainAddRequest
	if err := readJSON(r, &in); err != nil {
		return err
	}
	in.Hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.Hostname), "."))
	if strings.Contains(in.Hostname, "*") {
		return badRequest("wildcard domains are not supported — add each hostname explicitly")
	}
	if !hostRe.MatchString(in.Hostname) {
		return badRequest("%q is not a valid hostname", in.Hostname)
	}
	dm := store.Domain{StageID: st.ID, Hostname: in.Hostname, Service: in.Service, Port: in.Port, RedirectTo: strings.ToLower(in.RedirectTo)}
	var warnings []string
	if dm.RedirectTo == "" {
		plan, err := d.currentPlan(st)
		if err != nil {
			return err
		}
		if plan != nil {
			if dm.Service == "" {
				// Default: the only stateless service that declares a port.
				var cands []string
				for _, s := range plan.Services {
					if !s.Stateful && !s.OneShot && len(s.Ports) > 0 {
						cands = append(cands, s.Name)
					}
				}
				if len(cands) != 1 {
					return badRequest("which service should %s route to? pass --service (candidates: %s)", in.Hostname, strings.Join(serviceNames(plan), ", "))
				}
				dm.Service = cands[0]
			}
			svc, ok := plan.Service(dm.Service)
			if !ok {
				return badRequest("service %q is not in the current release (services: %s)", dm.Service, strings.Join(serviceNames(plan), ", "))
			}
			if dm.Port == 0 && len(svc.Ports) == 0 {
				warnings = append(warnings, dm.Service+" declares no port (ports/expose): routing to 80 — pass --port if it listens elsewhere")
			}
		} else if dm.Service == "" {
			return badRequest("the app is not deployed yet: pass --service")
		}
		// Port 0 = auto: resolved from the service's declared ports at each release.
	}
	if err := d.store.AddDomain(dm); err != nil {
		return &httpError{status: http.StatusConflict, msg: err.Error()}
	}
	d.refreshHosts()
	if err := d.engine.ApplyRoutes(r.Context()); err != nil {
		warnings = append(warnings, "proxy not updated yet: "+err.Error())
	}
	plan, _ := d.currentPlan(st)
	out := api.Domain{Hostname: dm.Hostname, Stage: st.Name, Service: dm.Service, Port: d.domainPort(plan, dm), RedirectTo: dm.RedirectTo, Check: d.checkDomain(r.Context(), dm.Hostname)}
	return writeJSON(w, struct {
		api.Domain
		Warnings []string `json:"warnings,omitempty"`
	}{out, warnings})
}

// domainPort resolves the port shown for a domain (0 = not known yet).
func (d *Daemon) domainPort(plan *compose.Plan, dm store.Domain) int {
	if dm.RedirectTo != "" {
		return 0
	}
	if plan == nil {
		return dm.Port
	}
	svc, _ := plan.Service(dm.Service)
	return compose.ResolvePort(dm.Port, svc.Ports)
}

func serviceNames(p *compose.Plan) []string {
	var out []string
	for _, s := range p.Services {
		out = append(out, s.Name)
	}
	return out
}

func (d *Daemon) removeDomain(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	if err := d.store.RemoveDomain(st.ID, r.PathValue("host")); err != nil {
		return err
	}
	d.refreshHosts()
	return writeJSON(w, map[string]any{"status": "removed", "proxy_error": errString(d.engine.ApplyRoutes(r.Context()))})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---- lifecycle ----

func (d *Daemon) restart(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	n, err := d.engine.Restart(r.Context(), st.App, st.Name, r.URL.Query().Get("service"))
	if err != nil {
		return err
	}
	return writeJSON(w, map[string]int{"restarted": n})
}

func (d *Daemon) stopStage(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	if err := d.engine.Stop(r.Context(), st.App, st.Name); err != nil {
		return err
	}
	return writeJSON(w, map[string]string{"status": "stopped"})
}

func (d *Daemon) startStage(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	if err := d.engine.Start(r.Context(), st.App, st.Name); err != nil {
		return err
	}
	d.Kick()
	return writeJSON(w, map[string]string{"status": "started"})
}

// ---- logs ----

func (d *Daemon) logs(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	follow := q.Get("follow") == "1"
	service := q.Get("service")
	grep := strings.ToLower(q.Get("grep"))
	tail := q.Get("tail")
	if tail == "" {
		tail = "100"
	}
	var since int64
	if s := q.Get("since"); s != "" {
		dur, err := time.ParseDuration(s)
		if err != nil {
			return badRequest("invalid since %q (use e.g. 30m, 2h)", s)
		}
		since = time.Now().Add(-dur).Unix()
		tail = "all"
	}
	ctx := r.Context()
	list := func() ([]docker.ContainerSummary, error) {
		cs, err := d.docker.Containers(ctx, compose.LabelApp+"="+st.App, compose.LabelStage+"="+st.Name)
		if err != nil {
			return nil, err
		}
		cur, _ := d.store.GetStage(st.App, st.Name)
		var out []docker.ContainerSummary
		for _, c := range cs {
			l := c.Labels
			if service != "" && l[compose.LabelService] != service {
				continue
			}
			if l[compose.LabelRole] == "app" && l[compose.LabelColor] != cur.ActiveColor && !follow {
				continue
			}
			out = append(out, c)
		}
		return out, nil
	}
	cs, err := list()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	fl, _ := w.(http.Flusher)
	write := func(l api.LogLine) {
		if grep != "" && !strings.Contains(strings.ToLower(l.Line), grep) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(l)
		if fl != nil {
			fl.Flush()
		}
	}
	replica := func(c docker.ContainerSummary) int {
		n, _ := strconv.Atoi(c.Labels["com.docker.compose.container-number"])
		return n
	}

	// History, merged by time.
	var hist []api.LogLine
	var hmu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func(c docker.ContainerSummary) {
			defer wg.Done()
			ci, err := d.docker.Inspect(ctx, c.ID)
			if err != nil {
				return
			}
			_ = d.docker.Logs(ctx, c.ID, ci.Config.Tty, docker.LogOptions{Tail: tail, Since: since}, func(stream string, t time.Time, line string) {
				hmu.Lock()
				hist = append(hist, api.LogLine{Time: t, Service: c.Labels[compose.LabelService], Replica: replica(c), Stream: stream, Line: line})
				hmu.Unlock()
			})
		}(c)
	}
	wg.Wait()
	sort.SliceStable(hist, func(i, j int) bool { return hist[i].Time.Before(hist[j].Time) })
	if n, err := strconv.Atoi(tail); err == nil && len(hist) > n {
		hist = hist[len(hist)-n:]
	}
	for _, l := range hist {
		write(l)
	}
	if !follow {
		return nil
	}

	// Follow: attach to current containers and to new ones as they appear.
	attached := map[string]bool{}
	start := time.Now().Unix()
	attach := func(c docker.ContainerSummary, from int64) {
		attached[c.ID] = true
		go func() {
			ci, err := d.docker.Inspect(ctx, c.ID)
			if err != nil {
				return
			}
			_ = d.docker.Logs(ctx, c.ID, ci.Config.Tty, docker.LogOptions{Follow: true, Since: from, Tail: "0"}, func(stream string, t time.Time, line string) {
				write(api.LogLine{Time: t, Service: c.Labels[compose.LabelService], Replica: replica(c), Stream: stream, Line: line})
			})
		}()
	}
	for _, c := range cs {
		attach(c, start)
	}
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			cs, err := list()
			if err != nil {
				continue
			}
			for _, c := range cs {
				if !attached[c.ID] {
					attach(c, 0)
				}
			}
		}
	}
}

// ---- db:connect & exec ----

var dbPorts = map[string]int{"postgres": 5432, "postgis": 5432, "timescaledb": 5432, "mysql": 3306, "mariadb": 3306, "percona": 3306, "mongo": 27017, "redis": 6379, "valkey": 6379, "keydb": 6379, "dragonfly": 6379}

func dbKind(image string) string {
	switch k := compose.ImageKind(image); k {
	case "postgis", "timescaledb":
		return "postgres"
	case "mariadb", "percona":
		return "mysql"
	case "valkey", "keydb", "dragonfly":
		return "redis"
	default:
		if _, ok := dbPorts[k]; ok {
			return k
		}
		return "unknown"
	}
}

func (d *Daemon) dbTarget(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	plan, err := d.currentPlan(st)
	if err != nil {
		return err
	}
	if plan == nil {
		return badRequest("%s is not deployed", st.Key())
	}
	service := r.URL.Query().Get("service")
	port, _ := strconv.Atoi(r.URL.Query().Get("port"))
	if service == "" && len(dbCandidates(plan)) == 0 && len(plan.Uses) > 0 {
		t, err := d.sharedDBTarget(r.Context(), st, plan, port)
		if err != nil {
			return err
		}
		return writeJSON(w, t)
	}
	t, err := d.dbTargetOf(r.Context(), st, plan, service, port)
	if err != nil {
		return err
	}
	return writeJSON(w, t)
}

// dbCandidates lists a plan's database services, SQL databases before caches.
func dbCandidates(plan *compose.Plan) []string {
	var cands []string
	for _, s := range plan.Services {
		if dbKind(s.Image) != "unknown" {
			cands = append(cands, s.Name)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		si, _ := plan.Service(cands[i])
		return dbKind(si.Image) != "redis"
	})
	return cands
}

// dbTargetOf resolves a database service of a stage, with the credentials
// found in its container's environment.
func (d *Daemon) dbTargetOf(ctx context.Context, st store.Stage, plan *compose.Plan, service string, port int) (api.DBTarget, error) {
	if service == "" {
		cands := dbCandidates(plan)
		if len(cands) == 0 {
			return api.DBTarget{}, badRequest("no database service detected — pass --service")
		}
		service = cands[0]
	}
	svc, ok := plan.Service(service)
	if !ok {
		return api.DBTarget{}, badRequest("service %q not found", service)
	}
	containers, err := d.engine.StageContainers(ctx, st)
	if err != nil {
		return api.DBTarget{}, err
	}
	cs := containers[service]
	if len(cs) == 0 || cs[0].State != "running" {
		return api.DBTarget{}, badRequest("service %s is not running", service)
	}
	ci, err := d.docker.Inspect(ctx, cs[0].ID)
	if err != nil {
		return api.DBTarget{}, err
	}
	kind := dbKind(svc.Image)
	if port == 0 {
		port = dbPorts[compose.ImageKind(svc.Image)]
	}
	if port == 0 && len(svc.Ports) > 0 {
		port = svc.Ports[0]
	}
	if port == 0 {
		return api.DBTarget{}, badRequest("unknown port for %s — pass --remote-port", service)
	}
	env := ci.EnvMap()
	t := api.DBTarget{Service: service, Kind: kind, Address: fmt.Sprintf("%s:%d", cs[0].IP, port), Container: cs[0].Name}
	switch kind {
	case "postgres":
		t.User = firstSet(env["POSTGRES_USER"], "postgres")
		t.Password = env["POSTGRES_PASSWORD"]
		t.Database = firstSet(env["POSTGRES_DB"], t.User)
	case "mysql":
		if u := firstSet(env["MYSQL_USER"], env["MARIADB_USER"]); u != "" {
			t.User, t.Password = u, firstSet(env["MYSQL_PASSWORD"], env["MARIADB_PASSWORD"])
		} else {
			t.User, t.Password = "root", firstSet(env["MYSQL_ROOT_PASSWORD"], env["MARIADB_ROOT_PASSWORD"])
		}
		t.Database = firstSet(env["MYSQL_DATABASE"], env["MARIADB_DATABASE"])
	case "mongo":
		t.User, t.Password = env["MONGO_INITDB_ROOT_USERNAME"], env["MONGO_INITDB_ROOT_PASSWORD"]
		t.Database = env["MONGO_INITDB_DATABASE"]
	case "redis":
		t.Password = firstSet(env["REDIS_PASSWORD"], env["VALKEY_PASSWORD"])
	}
	return t, nil
}

func firstSet(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func (d *Daemon) execTarget(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	containers, err := d.engine.StageContainers(r.Context(), st)
	if err != nil {
		return err
	}
	service := r.URL.Query().Get("service")
	if service == "" {
		plan, _ := d.currentPlan(st)
		if plan != nil {
			for _, s := range plan.Services {
				if !s.Stateful && !s.OneShot {
					service = s.Name
					break
				}
			}
		}
	}
	for _, c := range containers[service] {
		if c.State == "running" {
			return writeJSON(w, api.ExecTarget{Container: c.Name})
		}
	}
	return badRequest("no running container for service %q", service)
}

// ---- metrics ----

func (d *Daemon) getTop(w http.ResponseWriter, r *http.Request) error {
	return writeJSON(w, d.collector.Snapshot())
}

func parseSince(r *http.Request) (since, step int64, err error) {
	s := r.URL.Query().Get("since")
	if s == "" {
		s = "24h"
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return 0, 0, badRequest("invalid since %q", s)
	}
	step = int64(dur.Seconds()) / 60 // ~60 points
	if step < 60 {
		step = 60
	}
	return time.Now().Add(-dur).Unix(), step, nil
}

func (d *Daemon) getMetrics(w http.ResponseWriter, r *http.Request) error {
	since, step, err := parseSince(r)
	if err != nil {
		return err
	}
	h, err := d.store.MetricsHistory(nil, nil, since, step)
	if err != nil {
		return err
	}
	return writeJSON(w, h)
}

func (d *Daemon) stageMetrics(w http.ResponseWriter, r *http.Request) error {
	st, err := d.stageOf(r)
	if err != nil {
		return err
	}
	since, step, err := parseSince(r)
	if err != nil {
		return err
	}
	doms, _ := d.store.Domains(st.ID)
	var hosts []string
	for _, dm := range doms {
		if dm.RedirectTo == "" {
			hosts = append(hosts, dm.Hostname)
		}
	}
	h, err := d.store.MetricsHistory([]int64{st.ID}, hosts, since, step)
	if err != nil {
		return err
	}
	h.App = st.App
	return writeJSON(w, h)
}

// ---- alerts ----

func (d *Daemon) getAlerts(w http.ResponseWriter, r *http.Request) error {
	chans, err := d.store.AlertChannels()
	if err != nil {
		return err
	}
	for i := range chans {
		chans[i].URL = redactURL(chans[i].URL)
	}
	firing := d.alerts.Firing()
	sort.Strings(firing)
	return writeJSON(w, api.AlertsConfig{Channels: chans, Thresholds: d.alerts.Thresholds(), Firing: firing})
}

// redactURL hides webhook secrets (the path) in listings.
func redactURL(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return "•••"
	}
	rest := u[i+3:]
	host, _, _ := strings.Cut(rest, "/")
	return u[:i+3] + host + "/•••"
}

func (d *Daemon) addAlertChannel(w http.ResponseWriter, r *http.Request) error {
	var in api.AlertChannel
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.Kind != "discord" && in.Kind != "slack" {
		return badRequest("channel kind must be discord or slack")
	}
	if !strings.HasPrefix(in.URL, "https://") {
		return badRequest("webhook URL must start with https://")
	}
	id, err := d.store.AddAlertChannel(in.Kind, in.URL)
	if err != nil {
		return err
	}
	return writeJSON(w, map[string]int64{"id": id})
}

func (d *Daemon) removeAlertChannel(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		return badRequest("invalid id")
	}
	if err := d.store.RemoveAlertChannel(id); err != nil {
		return err
	}
	return writeJSON(w, map[string]string{"status": "removed"})
}

func (d *Daemon) testAlerts(w http.ResponseWriter, r *http.Request) error {
	errs := d.alerts.Test()
	var msgs []string
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return writeJSON(w, map[string]any{"errors": msgs})
}

func (d *Daemon) setAlertSettings(w http.ResponseWriter, r *http.Request) error {
	var in map[string]string
	if err := readJSON(r, &in); err != nil {
		return err
	}
	for k, v := range in {
		if err := d.alerts.SetThreshold(k, v); err != nil {
			return badRequest("%v", err)
		}
	}
	return writeJSON(w, d.alerts.Thresholds())
}
