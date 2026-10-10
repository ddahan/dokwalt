package compose

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const sample = `{
  "name": "sample",
  "services": {
    "db": {
      "environment": {"POSTGRES_PASSWORD": "${DB_PASSWORD}"},
      "healthcheck": {"interval": "5s", "test": ["CMD", "pg_isready"]},
      "image": "postgres:17-alpine",
      "volumes": [{"source": "pgdata", "target": "/var/lib/postgresql/data", "type": "volume", "volume": {}}]
    },
    "migrate": {
      "build": {"context": "/Users/me/app", "dockerfile": "Dockerfile"},
      "command": ["npm", "run", "migrate"],
      "depends_on": {"db": {"condition": "service_healthy", "required": true}}
    },
    "web": {
      "build": {"context": "/Users/me/app", "dockerfile": "Dockerfile"},
      "container_name": "web",
      "depends_on": {
        "db": {"condition": "service_healthy", "required": true},
        "migrate": {"condition": "service_completed_successfully", "required": true}
      },
      "env_file": [{"path": "/Users/me/app/.env", "required": true}],
      "environment": {"DATABASE_URL": "postgres://app:${DB_PASSWORD}@db:5432/app", "PORT": 3000, "OPTIONAL": "${X:-fallback}"},
      "ports": [{"mode": "ingress", "protocol": "tcp", "published": "3000", "target": 3000}]
    },
    "cache": {"image": "redis:7"}
  },
  "volumes": {"pgdata": {"name": "sample_pgdata"}}
}`

func load(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(sample), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTransform(t *testing.T) {
	plan, err := Transform(Input{
		App: "shop", Stage: "production", Release: 3, Compose: load(t),
		Images:     map[string]string{"web": "dokwalt/shop-web:1", "migrate": "dokwalt/shop-web:1"},
		ConfigKeys: []string{"SECRET_KEY"},
		ProjectDir: "/Users/me/app",
	})
	if err != nil {
		t.Fatal(err)
	}
	stateful := map[string]bool{}
	for _, s := range plan.Services {
		stateful[s.Name] = s.Stateful
	}
	want := map[string]bool{"db": true, "cache": true, "web": false, "migrate": false}
	if !reflect.DeepEqual(stateful, want) {
		t.Fatalf("stateful = %v, want %v", stateful, want)
	}
	if plan.Data["name"] != "dw-shop-production-data" {
		t.Fatalf("data project name = %v", plan.Data["name"])
	}
	appDoc, _ := RenderApp(plan, "shop", "production", "green")
	if appDoc["name"] != "dw-shop-production-green" {
		t.Fatalf("app project name = %v", appDoc["name"])
	}
	web := appDoc["services"].(map[string]any)["web"].(map[string]any)
	for _, k := range []string{"ports", "container_name", "env_file", "build"} {
		if _, ok := web[k]; ok {
			t.Errorf("web still has %s", k)
		}
	}
	if web["image"] != "dokwalt/shop-web:1" {
		t.Errorf("web image = %v", web["image"])
	}
	deps := web["depends_on"].(map[string]any)
	if _, ok := deps["db"]; ok {
		t.Error("cross-project depends_on on db should be removed")
	}
	if _, ok := deps["migrate"]; !ok {
		t.Error("same-project depends_on on migrate should be kept")
	}
	env := web["environment"].(map[string]any)
	if v, ok := env["SECRET_KEY"]; !ok || v != nil {
		t.Errorf("config key not injected as pass-through: %v", env)
	}
	labels := web["labels"].(map[string]string)
	if labels[LabelColor] != "green" || labels[LabelRelease] != "3" {
		t.Errorf("labels = %v", labels)
	}
	migrate := appDoc["services"].(map[string]any)["migrate"].(map[string]any)
	if migrate["restart"] != "no" {
		t.Errorf("one-shot migrate restart = %v", migrate["restart"])
	}
	if !reflect.DeepEqual(plan.Volumes, []string{"dw-shop-production-pgdata"}) {
		t.Errorf("volumes = %v", plan.Volumes)
	}
	dbLabels := plan.Data["services"].(map[string]any)["db"].(map[string]any)["labels"].(map[string]string)
	if _, ok := dbLabels[LabelRelease]; ok {
		t.Error("stateful services must not carry the release label (it would recreate them every deploy)")
	}
	dbEnv := plan.Data["services"].(map[string]any)["db"].(map[string]any)["environment"].(map[string]any)
	if _, ok := dbEnv["SECRET_KEY"]; ok {
		t.Error("config must not be injected into stateful services")
	}
	if !reflect.DeepEqual(plan.Missing, []string{"DB_PASSWORD"}) {
		t.Errorf("missing = %v", plan.Missing)
	}
	if len(plan.Warnings) < 3 {
		t.Errorf("expected warnings, got %v", plan.Warnings)
	}
}

func TestRejectProjectBindMount(t *testing.T) {
	m := load(t)
	web := m["services"].(map[string]any)["web"].(map[string]any)
	web["volumes"] = []any{map[string]any{"type": "bind", "source": "/Users/me/app/uploads", "target": "/u"}}
	_, err := Transform(Input{App: "shop", Stage: "production", Compose: m,
		Images: map[string]string{"web": "x", "migrate": "x"}, ProjectDir: "/Users/me/app"})
	if err == nil || !strings.Contains(err.Error(), "uploads") {
		t.Fatalf("expected bind mount error, got %v", err)
	}
}

func TestStatefulOverride(t *testing.T) {
	f := false
	plan, err := Transform(Input{App: "shop", Stage: "production", Compose: load(t),
		Images: map[string]string{"web": "x", "migrate": "x"}, Stateful: map[string]*bool{"cache": &f}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := plan.Service("cache")
	if s.Stateful || !s.Overridden {
		t.Fatalf("override not applied: %+v", s)
	}
}

func TestReferencedVars(t *testing.T) {
	got := ReferencedVars(`$A ${B} ${C:-x} ${D-y} ${E:?err} $$F ${G:+z}`)
	want := []string{"A", "B", "E"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"shop", "my-blog", "a1"} {
		if ValidName(ok) != nil {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"", "Shop", "1app", "a_b", "trailing-", strings.Repeat("a", 31)} {
		if ValidName(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestUses(t *testing.T) {
	m := load(t)
	m["x-dokwalt"] = map[string]any{"uses": []any{"postgres", "search", "postgres"}, "other": true}
	plan, err := Transform(Input{App: "shop", Stage: "production", Compose: m,
		Images: map[string]string{"web": "w", "migrate": "w"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Uses, []string{"postgres", "search"}) {
		t.Fatalf("uses = %v", plan.Uses)
	}
	if !strings.Contains(strings.Join(plan.Warnings, "\n"), "x-dokwalt.other ignored") {
		t.Fatalf("missing warning for unknown key: %v", plan.Warnings)
	}
	// x-dokwalt never reaches the rendered projects.
	if _, ok := plan.Data["x-dokwalt"]; ok {
		t.Fatal("x-dokwalt leaked into the data project")
	}

	for name, uses := range map[string]any{
		"self":      []any{"shop"},
		"shadowing": []any{"cache"}, // a service of this app
		"invalid":   []any{"Bad_Name"},
		"not list":  "postgres",
	} {
		m := load(t)
		m["x-dokwalt"] = map[string]any{"uses": uses}
		if _, err := Transform(Input{App: "shop", Stage: "production", Compose: m,
			Images: map[string]string{"web": "w", "migrate": "w"}}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSharedAliases(t *testing.T) {
	if got := SharedAliases("postgres", "db", true); !reflect.DeepEqual(got, []string{"postgres", "postgres-db"}) {
		t.Fatalf("single: %v", got)
	}
	if got := SharedAliases("data", "redis", false); !reflect.DeepEqual(got, []string{"data-redis"}) {
		t.Fatalf("several: %v", got)
	}
}
