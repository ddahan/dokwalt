package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/secrets"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), secrets.NewForTest())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAppsAndPipeline(t *testing.T) {
	s := open(t)
	a, err := s.CreateApp("shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("shop"); err == nil {
		t.Fatal("duplicate app accepted")
	}
	if _, err := s.GetStage("shop", api.StagingStage); err == nil || !strings.Contains(err.Error(), "pipeline:enable") {
		t.Fatalf("staging without pipeline: %v", err)
	}
	if err := s.SetPipeline(a.ID, true); err != nil {
		t.Fatal(err)
	}
	stages, _ := s.Stages(a.ID)
	if len(stages) != 2 || stages[0].Name != api.DefaultStage {
		t.Fatalf("stages = %+v", stages)
	}
	if err := s.SetPipeline(a.ID, false); err != nil {
		t.Fatal(err)
	}
	if stages, _ := s.Stages(a.ID); len(stages) != 1 {
		t.Fatalf("staging not removed: %+v", stages)
	}
}

func TestConfigEncryptedAndVersioned(t *testing.T) {
	s := open(t)
	s.CreateApp("shop")
	st, _ := s.GetStage("shop", api.DefaultStage)
	v1, err := s.UpdateConfig(st.ID, map[string]string{"A": "1", "SECRET": "p@ss$word"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := s.UpdateConfig(st.ID, map[string]string{"A": "2"}, []string{"SECRET"})
	if v1 != 1 || v2 != 2 {
		t.Fatalf("versions %d %d", v1, v2)
	}
	cfg, _ := s.Config(st.ID)
	if cfg["A"] != "2" || cfg["SECRET"] != "" {
		t.Fatalf("config = %v", cfg)
	}
	old, _ := s.ConfigSnapshot(st.ID, 1)
	if old["SECRET"] != "p@ss$word" {
		t.Fatalf("snapshot = %v", old)
	}
	// Values are not stored in clear text.
	var raw []byte
	s.db.QueryRow(`SELECT value FROM config WHERE key = 'A'`).Scan(&raw)
	if strings.Contains(string(raw), "2") && len(raw) < 10 {
		t.Fatal("config stored in clear text")
	}
}

func TestReleasesNumbering(t *testing.T) {
	s := open(t)
	s.CreateApp("shop")
	st, _ := s.GetStage("shop", api.DefaultStage)
	for i := 1; i <= 3; i++ {
		r := &Release{StageID: st.ID, Status: StatusDeploying, Images: map[string]api.Image{"web": {Ref: "x"}}, Compose: map[string]any{"services": map[string]any{}}}
		if err := s.CreateRelease(r); err != nil {
			t.Fatal(err)
		}
		if r.Version != i {
			t.Fatalf("version = %d, want %d", r.Version, i)
		}
		s.FinishRelease(r.ID, StatusSucceeded, "")
	}
	s.SupersedeOthers(st.ID, 3)
	rels, _ := s.Releases(st.ID, 10)
	if rels[0].Version != 3 || rels[0].Status != StatusSucceeded || rels[1].Status != StatusSuperseded {
		t.Fatalf("releases = %+v", rels)
	}
}

// MetricsHistory must not deadlock (it used to nest queries on one connection).
func TestMetricsHistory(t *testing.T) {
	s := open(t)
	s.CreateApp("shop")
	st, _ := s.GetStage("shop", api.DefaultStage)
	now := time.Now().Unix()
	var cs []ContainerSample
	for i := 0; i < 10; i++ {
		cs = append(cs, ContainerSample{TS: now - int64(i*60), StageID: st.ID, Service: "web", CPUPct: float64(i), MemBytes: 1 << 20})
	}
	if err := s.InsertMetrics(cs, []HTTPSample{{TS: now, Hostname: "shop.test", Requests: 5}}, &HostSample{TS: now, CPUPct: 3}); err != nil {
		t.Fatal(err)
	}
	done := make(chan api.MetricsHistory, 1)
	go func() {
		h, err := s.MetricsHistory([]int64{st.ID}, []string{"shop.test"}, now-3600, 60)
		if err != nil {
			t.Error(err)
		}
		done <- h
	}()
	select {
	case h := <-done:
		if len(h.Services["web"]) != 10 || len(h.HTTP["shop.test"]) != 1 || len(h.Host) != 1 {
			t.Fatalf("history = %+v", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MetricsHistory deadlocked")
	}
}

func TestSecretSettingsAndCopy(t *testing.T) {
	s := open(t)
	if err := s.SetSecretSetting("backup_secret_key", "s3cr3t"); err != nil {
		t.Fatal(err)
	}
	if raw := s.Setting("backup_secret_key", ""); raw == "" || strings.Contains(raw, "s3cr3t") {
		t.Fatalf("stored in clear: %q", raw)
	}
	if v, err := s.SecretSetting("backup_secret_key"); err != nil || v != "s3cr3t" {
		t.Fatalf("got %q %v", v, err)
	}
	if v, err := s.SecretSetting("missing"); err != nil || v != "" {
		t.Fatalf("missing: %q %v", v, err)
	}

	_ = s.SetSetting("backup_bucket", "b")
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if err := s.CopyTo(copyPath); err != nil {
		t.Fatal(err)
	}
	c, err := Open(copyPath, secrets.NewForTest())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Setting("backup_bucket", "") != "b" {
		t.Fatal("copy is missing data")
	}

	if err := s.DeleteSettings("backup_"); err != nil {
		t.Fatal(err)
	}
	if s.Setting("backup_bucket", "gone") != "gone" {
		t.Fatal("settings not deleted")
	}
}
