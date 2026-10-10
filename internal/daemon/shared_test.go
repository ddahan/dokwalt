package daemon

import (
	"testing"

	"github.com/ddahan/dokwalt/internal/compose"
)

func TestConsumerDBURL(t *testing.T) {
	hosts := []string{"postgres", "postgres-db"}
	plan := &compose.Plan{App: map[string]any{"services": map[string]any{
		"web": map[string]any{"environment": map[string]any{
			"DATABASE_URL": "postgres://blog:${DB_PASSWORD:?set it}@postgres:5432/blog",
			"SECRET":       nil, // a config key
		}},
	}}}

	// From the compose environment, interpolated with the config.
	u, pw, db, ok := consumerDBURL("postgres", hosts, plan, map[string]string{"DB_PASSWORD": "s3cr3t"})
	if !ok || u != "blog" || pw != "s3cr3t" || db != "blog" {
		t.Fatalf("compose env: %q %q %q %v", u, pw, db, ok)
	}

	// A config var wins, DATABASE_URL first; URLs to other hosts are ignored.
	cfg := map[string]string{
		"ANALYTICS_URL": "postgres://x:y@elsewhere:5432/x",
		"DATABASE_URL":  "postgresql://shop:p%40ss@postgres-db/shop",
	}
	u, pw, db, ok = consumerDBURL("postgres", hosts, plan, cfg)
	if !ok || u != "shop" || pw != "p@ss" || db != "shop" {
		t.Fatalf("config: %q %q %q %v", u, pw, db, ok)
	}

	if _, _, _, ok := consumerDBURL("redis", hosts, plan, cfg); ok {
		t.Fatal("redis has no URL lookup")
	}
	if _, _, _, ok := consumerDBURL("postgres", []string{"db"}, &compose.Plan{}, nil); ok {
		t.Fatal("nothing to find")
	}
}

func TestExpandVars(t *testing.T) {
	cfg := map[string]string{"A": "1", "EMPTY": ""}
	for in, want := range map[string]string{
		"${A}":            "1",
		"$A-x":            "1-x",
		"${B:-def}":       "def",
		"${EMPTY:-def}":   "def",
		"${B?required}":   "",
		"pre${A}post${B}": "pre1post",
	} {
		if got := expandVars(in, cfg); got != want {
			t.Errorf("expandVars(%q) = %q, want %q", in, got, want)
		}
	}
}
