// Package compose turns a user's normalized compose model into the compose
// files DokWalt actually runs: one "data" project holding stateful services
// (never duplicated) and one "color" project (blue or green) holding
// stateless services, which is swapped on every deploy.
package compose

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Labels set on every container DokWalt manages.
const (
	LabelApp     = "dokwalt.app"
	LabelStage   = "dokwalt.stage"
	LabelService = "dokwalt.service"
	LabelRelease = "dokwalt.release"
	LabelColor   = "dokwalt.color"
	LabelRole    = "dokwalt.role" // data | app | system
	LabelOneShot = "dokwalt.oneshot"
	LabelPorts   = "dokwalt.ports" // container ports declared in the compose file
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}$`)

// ValidName checks app names: they end up in project, network and volume names.
func ValidName(name string) error {
	if !nameRe.MatchString(name) || strings.HasSuffix(name, "-") {
		return fmt.Errorf("invalid name %q: use 1-30 lowercase letters, digits and dashes, starting with a letter", name)
	}
	return nil
}

func Network(app, stage string) string     { return "dw-" + app + "-" + stage }
func DataProject(app, stage string) string { return "dw-" + app + "-" + stage + "-data" }
func ColorProject(app, stage, color string) string {
	return "dw-" + app + "-" + stage + "-" + color
}
func VolumeName(app, stage, vol string) string { return "dw-" + app + "-" + stage + "-" + vol }

// ResolvePort picks the port a domain routes to: the explicit one, else the
// first port the service declares, else 80.
func ResolvePort(explicit int, declared []int) int {
	if explicit > 0 {
		return explicit
	}
	if len(declared) > 0 {
		return declared[0]
	}
	return 80
}

// PortsFromLabel parses the LabelPorts value.
func PortsFromLabel(v string) []int {
	var out []int
	for _, f := range strings.Split(v, ",") {
		var n int
		if _, err := fmt.Sscan(f, &n); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func OtherColor(c string) string {
	if c == "blue" {
		return "green"
	}
	return "blue"
}

// Well-known images whose containers hold state.
var statefulImages = []string{
	"postgres", "postgis", "timescaledb", "mysql", "mariadb", "percona", "mongo", "redis", "valkey",
	"keydb", "dragonfly", "memcached", "rabbitmq", "elasticsearch", "opensearch", "clickhouse",
	"influxdb", "minio", "cassandra", "couchdb", "neo4j", "nats", "meilisearch", "typesense", "qdrant",
}

// ImageKind returns the base name of an image ("postgres" for "docker.io/library/postgres:17").
func ImageKind(image string) string {
	base := image
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.IndexAny(base, ":@"); i >= 0 {
		base = base[:i]
	}
	return base
}

func isStatefulImage(image string) bool {
	kind := ImageKind(image)
	for _, s := range statefulImages {
		if kind == s || strings.HasPrefix(kind, s+"-") {
			return true
		}
	}
	return false
}

// Input describes one transformation.
type Input struct {
	App, Stage string
	Release    int
	Compose    map[string]any    // normalized, un-interpolated compose model
	Images     map[string]string // built service -> image ref to run
	Stateful   map[string]*bool  // per-service overrides
	ConfigKeys []string          // config var names, injected into stateless services
	ProjectDir string            // client-side project dir, to reject relative bind mounts
}

// ServiceInfo summarizes how DokWalt treats a service.
type ServiceInfo struct {
	Name        string
	Image       string
	Stateful    bool
	Reason      string
	Overridden  bool
	Healthcheck bool
	OneShot     bool
	Ports       []int // container ports declared in the original compose file
}

// Plan is the result of a transformation.
type Plan struct {
	Services []ServiceInfo
	Data     map[string]any // nil when there are no stateful services
	App      map[string]any // nil when there are no stateless services; project name set by Render
	Volumes  []string       // volume names DokWalt must create before `up`
	Uses     []string       // apps whose stateful services join this stage's network (x-dokwalt.uses)
	Warnings []string
	Missing  []string // ${VARS} referenced without default and absent from config
}

func (p Plan) Service(name string) (ServiceInfo, bool) {
	for _, s := range p.Services {
		if s.Name == name {
			return s, true
		}
	}
	return ServiceInfo{}, false
}

// Transform validates and rewrites the compose model. The color project is
// returned without a name; call RenderApp to bind it to a color.
func Transform(in Input) (Plan, error) {
	var plan Plan
	src, err := deepCopy(in.Compose)
	if err != nil {
		return plan, err
	}
	services, _ := src["services"].(map[string]any)
	if len(services) == 0 {
		return plan, fmt.Errorf("compose file has no services")
	}
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)

	topVolumes, _ := src["volumes"].(map[string]any)
	netName := Network(in.App, in.Stage)
	warn := func(f string, a ...any) { plan.Warnings = append(plan.Warnings, fmt.Sprintf(f, a...)) }

	uses, unknown, err := Uses(src)
	if err != nil {
		return plan, err
	}
	for _, k := range unknown {
		warn("x-dokwalt.%s ignored (only `uses` is supported)", k)
	}
	for _, u := range uses {
		if u == in.App {
			return plan, fmt.Errorf("x-dokwalt.uses: an app can't use itself")
		}
		// The provider joins this network under its app name, which must not
		// shadow one of this app's own services.
		if _, ok := services[u]; ok {
			return plan, fmt.Errorf("x-dokwalt.uses: %q is also a service of this app — rename the service, it would hide the shared one", u)
		}
	}
	plan.Uses = uses

	// One-shot jobs: something depends on them with service_completed_successfully.
	oneShot := map[string]bool{}
	for _, n := range names {
		svc, _ := services[n].(map[string]any)
		deps, _ := svc["depends_on"].(map[string]any)
		for dep, v := range deps {
			if m, ok := v.(map[string]any); ok && m["condition"] == "service_completed_successfully" {
				oneShot[dep] = true
			}
		}
	}

	infos := map[string]*ServiceInfo{}
	usedVolumes := map[string]bool{}
	for _, n := range names {
		svc, ok := services[n].(map[string]any)
		if !ok {
			return plan, fmt.Errorf("service %s: invalid definition", n)
		}
		info := &ServiceInfo{Name: n, OneShot: oneShot[n]}
		infos[n] = info

		// Image: built on the laptop, or pulled on the server.
		if _, hasBuild := svc["build"]; hasBuild {
			ref, ok := in.Images[n]
			if !ok {
				return plan, fmt.Errorf("service %s has a build section but no image was uploaded for it", n)
			}
			delete(svc, "build")
			svc["image"] = ref
		} else if ref, ok := in.Images[n]; ok {
			svc["image"] = ref
		}
		img, _ := svc["image"].(string)
		if img == "" {
			return plan, fmt.Errorf("service %s has neither image nor build", n)
		}
		info.Image = img
		svc["pull_policy"] = "missing"

		// Ports: only the reverse proxy is exposed. Remember container ports.
		if ports, ok := svc["ports"].([]any); ok && len(ports) > 0 {
			for _, p := range ports {
				if pm, ok := p.(map[string]any); ok {
					if t, ok := toInt(pm["target"]); ok {
						info.Ports = append(info.Ports, t)
					}
				}
			}
			warn("%s: `ports` ignored — only the proxy is public (domains:add, db:connect)", n)
			delete(svc, "ports")
		}
		if exp, ok := svc["expose"].([]any); ok {
			for _, e := range exp {
				if t, ok := toInt(e); ok {
					info.Ports = append(info.Ports, t)
				}
			}
		}
		if _, ok := svc["container_name"]; ok {
			warn("%s: container_name removed (DokWalt names containers itself)", n)
			delete(svc, "container_name")
		}
		if _, ok := svc["env_file"]; ok {
			warn("%s: env_file ignored — config lives on the server; import it with `dokwalt config:import .env`", n)
			delete(svc, "env_file")
		}
		for _, k := range []string{"links", "external_links"} {
			if _, ok := svc[k]; ok {
				warn("%s: %s ignored (services reach each other by name)", n, k)
				delete(svc, k)
			}
		}
		if nm, ok := svc["network_mode"].(string); ok && nm != "" && nm != "bridge" {
			return plan, fmt.Errorf("service %s: network_mode %q is not supported", n, nm)
		}
		for _, k := range []string{"secrets", "configs"} {
			if _, ok := svc[k]; ok {
				return plan, fmt.Errorf("service %s: compose %s are not supported; use `dokwalt config:set` instead", n, k)
			}
		}
		if _, ok := svc["healthcheck"]; ok {
			if hc, _ := svc["healthcheck"].(map[string]any); hc["disable"] != true {
				info.Healthcheck = true
			}
		}

		// Volumes.
		if vols, ok := svc["volumes"].([]any); ok {
			for _, v := range vols {
				vm, ok := v.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := vm["type"].(string)
				source, _ := vm["source"].(string)
				switch typ {
				case "bind":
					if in.ProjectDir != "" && (source == in.ProjectDir || strings.HasPrefix(source, strings.TrimSuffix(in.ProjectDir, "/")+"/")) {
						return plan, fmt.Errorf("service %s: bind mount of a project file (%s) is not possible on the server — bake it into the image or use a named volume", n, strings.TrimPrefix(source, in.ProjectDir+"/"))
					}
					if b, ok := vm["bind"].(map[string]any); ok {
						delete(b, "create_host_path")
					}
					warn("%s: host path %s must exist on the server", n, source)
				case "volume":
					if source != "" {
						usedVolumes[source] = true
						if info.Reason == "" {
							info.Stateful, info.Reason = true, "named volume "+source
						}
					}
				}
			}
		}
		if info.Reason == "" && isStatefulImage(img) {
			info.Stateful, info.Reason = true, "database image "+ImageKind(img)
		}
		if info.Reason == "" {
			info.Reason = "no named volume, not a database image"
		}
		if ov, ok := in.Stateful[n]; ok && ov != nil {
			info.Stateful, info.Overridden = *ov, true
			info.Reason = "set with services:set"
		}

		// Networking: one private network per app stage, service name as alias.
		svc["networks"] = map[string]any{"dokwalt": map[string]any{"aliases": []any{n}}}

		if _, ok := svc["restart"]; !ok {
			if info.OneShot {
				svc["restart"] = "no"
			} else {
				svc["restart"] = "unless-stopped"
			}
		}
		if _, ok := svc["logging"]; !ok {
			svc["logging"] = map[string]any{"driver": "local", "options": map[string]any{"max-size": "10m", "max-file": "3"}}
		}
	}

	// Split services, fix cross-project depends_on, inject config.
	data := map[string]any{}
	app := map[string]any{}
	for _, n := range names {
		svc := services[n].(map[string]any)
		info := infos[n]
		labels := toStringMap(svc["labels"])
		labels[LabelApp] = in.App
		labels[LabelStage] = in.Stage
		labels[LabelService] = n
		if !info.Stateful {
			// Stateful services must keep an identical definition across
			// releases, or compose would recreate the database every deploy.
			labels[LabelRelease] = fmt.Sprint(in.Release)
		}
		if info.OneShot {
			labels[LabelOneShot] = "true"
		}
		if len(info.Ports) > 0 {
			ps := make([]string, len(info.Ports))
			for i, p := range info.Ports {
				ps[i] = fmt.Sprint(p)
			}
			labels[LabelPorts] = strings.Join(ps, ",")
		}
		if deps, ok := svc["depends_on"].(map[string]any); ok {
			for dep := range deps {
				if d, ok := infos[dep]; ok && d.Stateful != info.Stateful {
					delete(deps, dep)
				}
			}
			if len(deps) == 0 {
				delete(svc, "depends_on")
			}
		}
		if info.Stateful {
			labels[LabelRole] = "data"
			svc["labels"] = labels
			data[n] = svc
		} else {
			labels[LabelRole] = "app"
			svc["labels"] = labels
			env := toEnvMap(svc["environment"])
			for _, k := range in.ConfigKeys {
				env[k] = nil // value comes from the compose process environment
			}
			if len(env) > 0 {
				svc["environment"] = env
			}
			app[n] = svc
		}
	}

	// Named volumes: stable names shared by blue, green and data projects.
	vols := map[string]any{}
	for name := range usedVolumes {
		def, _ := topVolumes[name].(map[string]any)
		if ext, _ := def["external"].(bool); ext {
			vols[name] = def
			continue
		}
		vn := VolumeName(in.App, in.Stage, name)
		vols[name] = map[string]any{"name": vn, "external": true}
		plan.Volumes = append(plan.Volumes, vn)
	}
	sort.Strings(plan.Volumes)

	doc := func(svcs map[string]any) map[string]any {
		d := map[string]any{
			"services": svcs,
			"networks": map[string]any{"dokwalt": map[string]any{"name": netName, "external": true}},
		}
		// Only declare the volumes this project uses.
		used := map[string]any{}
		for _, s := range svcs {
			for _, v := range asSlice(s.(map[string]any)["volumes"]) {
				if vm, ok := v.(map[string]any); ok && vm["type"] == "volume" {
					if src, _ := vm["source"].(string); src != "" {
						used[src] = vols[src]
					}
				}
			}
		}
		if len(used) > 0 {
			d["volumes"] = used
		}
		return d
	}
	if len(data) > 0 {
		plan.Data = doc(data)
		plan.Data["name"] = DataProject(in.App, in.Stage)
	}
	if len(app) > 0 {
		plan.App = doc(app)
	}
	for _, n := range names {
		plan.Services = append(plan.Services, *infos[n])
	}

	// ${VARS} referenced without a default and not configured.
	have := map[string]bool{}
	for _, k := range in.ConfigKeys {
		have[k] = true
	}
	raw, _ := json.Marshal(services)
	for _, v := range ReferencedVars(string(raw)) {
		if !have[v] {
			plan.Missing = append(plan.Missing, v)
		}
	}
	return plan, nil
}

// Uses reads the top-level `x-dokwalt: {uses: [app, …]}` extension: other
// apps whose stateful services (e.g. a shared Postgres) this app reaches by
// their app name. It returns the sorted app names and any unknown keys.
func Uses(model map[string]any) (uses, unknown []string, err error) {
	raw, ok := model["x-dokwalt"]
	if !ok || raw == nil {
		return nil, nil, nil
	}
	ext, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("x-dokwalt must be a mapping, e.g. `x-dokwalt: {uses: [postgres]}`")
	}
	for k := range ext {
		if k != "uses" {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	list, ok := ext["uses"].([]any)
	if !ok && ext["uses"] != nil {
		return nil, unknown, fmt.Errorf("x-dokwalt.uses must be a list of app names, e.g. `uses: [postgres]`")
	}
	seen := map[string]bool{}
	for _, v := range list {
		name, _ := v.(string)
		if err := ValidName(name); err != nil {
			return nil, unknown, fmt.Errorf("x-dokwalt.uses: %w", err)
		}
		if !seen[name] {
			seen[name] = true
			uses = append(uses, name)
		}
	}
	sort.Strings(uses)
	return uses, unknown, nil
}

// SharedAliases are the names a provider's stateful service gets on a
// consumer's network: always "<app>-<service>", plus "<app>" alone when the
// provider has a single stateful service (the common case: postgres:5432).
func SharedAliases(provider, service string, single bool) []string {
	if single {
		return []string{provider, provider + "-" + service}
	}
	return []string{provider + "-" + service}
}

// RenderApp returns the color project for a given color.
func RenderApp(plan Plan, app, stage, color string) (map[string]any, error) {
	if plan.App == nil {
		return nil, nil
	}
	d, err := deepCopy(plan.App)
	if err != nil {
		return nil, err
	}
	d["name"] = ColorProject(app, stage, color)
	for _, s := range d["services"].(map[string]any) {
		svc := s.(map[string]any)
		labels := toStringMap(svc["labels"])
		labels[LabelColor] = color
		svc["labels"] = labels
	}
	return d, nil
}

var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)((?::?[-?+])[^}]*)?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// ReferencedVars lists ${VAR} references that have no default value.
func ReferencedVars(s string) []string {
	s = strings.ReplaceAll(s, "$$", "")
	seen := map[string]bool{}
	var out []string
	for _, m := range varRe.FindAllStringSubmatch(s, -1) {
		name := m[1]
		if name == "" {
			name = m[3]
		}
		if strings.HasPrefix(m[2], ":-") || strings.HasPrefix(m[2], "-") || strings.HasPrefix(m[2], ":+") || strings.HasPrefix(m[2], "+") {
			continue
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ---- helpers ----

func deepCopy(m map[string]any) (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case string:
		var n int
		_, err := fmt.Sscan(strings.Split(t, "/")[0], &n)
		return n, err == nil
	}
	return 0, false
}

func toStringMap(v any) map[string]string {
	out := map[string]string{}
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			out[k] = fmt.Sprint(val)
		}
	case []any:
		for _, e := range t {
			k, val, _ := strings.Cut(fmt.Sprint(e), "=")
			out[k] = val
		}
	}
	return out
}

func toEnvMap(v any) map[string]any {
	out := map[string]any{}
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			out[k] = val
		}
	case []any:
		for _, e := range t {
			k, val, ok := strings.Cut(fmt.Sprint(e), "=")
			if ok {
				out[k] = val
			} else {
				out[k] = nil
			}
		}
	}
	return out
}
