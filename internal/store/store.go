// Package store persists DokWalt's desired state in SQLite (WAL mode).
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dokwalt/dokwalt/internal/api"
	"github.com/dokwalt/dokwalt/internal/secrets"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db  *sql.DB
	box *secrets.Box
}

const schema = `
CREATE TABLE IF NOT EXISTS apps (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  pipeline INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS stages (
  id INTEGER PRIMARY KEY,
  app_id INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  active_color TEXT NOT NULL DEFAULT '',
  current_release INTEGER NOT NULL DEFAULT 0,
  config_version INTEGER NOT NULL DEFAULT 0,
  UNIQUE(app_id, name)
);
CREATE TABLE IF NOT EXISTS releases (
  id INTEGER PRIMARY KEY,
  stage_id INTEGER NOT NULL REFERENCES stages(id) ON DELETE CASCADE,
  version INTEGER NOT NULL,
  status TEXT NOT NULL,
  images TEXT NOT NULL,
  compose TEXT NOT NULL,
  config_version INTEGER NOT NULL,
  color TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  git_sha TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  finished_at INTEGER,
  error TEXT NOT NULL DEFAULT '',
  UNIQUE(stage_id, version)
);
CREATE TABLE IF NOT EXISTS config (
  stage_id INTEGER NOT NULL REFERENCES stages(id) ON DELETE CASCADE,
  key TEXT NOT NULL,
  value BLOB NOT NULL,
  PRIMARY KEY(stage_id, key)
);
CREATE TABLE IF NOT EXISTS config_versions (
  stage_id INTEGER NOT NULL REFERENCES stages(id) ON DELETE CASCADE,
  version INTEGER NOT NULL,
  snapshot BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY(stage_id, version)
);
CREATE TABLE IF NOT EXISTS domains (
  id INTEGER PRIMARY KEY,
  stage_id INTEGER NOT NULL REFERENCES stages(id) ON DELETE CASCADE,
  hostname TEXT NOT NULL UNIQUE,
  service TEXT NOT NULL DEFAULT '',
  port INTEGER NOT NULL DEFAULT 0,
  redirect_to TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS service_overrides (
  stage_id INTEGER NOT NULL REFERENCES stages(id) ON DELETE CASCADE,
  service TEXT NOT NULL,
  stateful INTEGER,
  health_path TEXT NOT NULL DEFAULT '',
  health_timeout_s INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(stage_id, service)
);
CREATE TABLE IF NOT EXISTS metrics_containers (
  ts INTEGER NOT NULL, stage_id INTEGER NOT NULL, service TEXT NOT NULL,
  cpu_pct REAL, mem_bytes INTEGER, net_rx INTEGER, net_tx INTEGER, blk_r INTEGER, blk_w INTEGER
);
CREATE INDEX IF NOT EXISTS metrics_containers_ts ON metrics_containers(stage_id, ts);
CREATE TABLE IF NOT EXISTS metrics_http (
  ts INTEGER NOT NULL, hostname TEXT NOT NULL, requests INTEGER, s2xx INTEGER, s3xx INTEGER,
  s4xx INTEGER, s5xx INTEGER, p50_ms REAL, p95_ms REAL, p99_ms REAL
);
CREATE INDEX IF NOT EXISTS metrics_http_ts ON metrics_http(hostname, ts);
CREATE TABLE IF NOT EXISTS metrics_host (
  ts INTEGER NOT NULL, cpu_pct REAL, mem_used INTEGER, mem_total INTEGER,
  disk_used INTEGER, disk_total INTEGER, load1 REAL, temp_c REAL
);
CREATE INDEX IF NOT EXISTS metrics_host_ts ON metrics_host(ts);
CREATE TABLE IF NOT EXISTS alert_channels (id INTEGER PRIMARY KEY, kind TEXT NOT NULL, url TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS alert_state (key TEXT PRIMARY KEY, firing INTEGER NOT NULL, last_sent INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

func Open(path string, box *secrets.Box) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-1024)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A few connections: WAL lets readers run beside the writer, and a read
	// issued while another query is open can never deadlock the pool.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, box: box}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---- apps & stages ----

type App struct {
	ID        int64
	Name      string
	Pipeline  bool
	CreatedAt time.Time
}

type Stage struct {
	ID             int64
	AppID          int64
	App            string
	Name           string
	ActiveColor    string
	CurrentRelease int
	ConfigVersion  int
}

func (st Stage) Key() string { return st.App + "/" + st.Name }

func (s *Store) CreateApp(name string) (App, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return App{}, err
	}
	defer tx.Rollback()
	now := time.Now()
	res, err := tx.Exec(`INSERT INTO apps(name, created_at) VALUES(?, ?)`, name, now.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return App{}, fmt.Errorf("app %q already exists", name)
		}
		return App{}, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO stages(app_id, name) VALUES(?, ?)`, id, api.DefaultStage); err != nil {
		return App{}, err
	}
	return App{ID: id, Name: name, CreatedAt: now}, tx.Commit()
}

func (s *Store) GetApp(name string) (App, error) {
	var a App
	var created int64
	err := s.db.QueryRow(`SELECT id, name, pipeline, created_at FROM apps WHERE name = ?`, name).
		Scan(&a.ID, &a.Name, &a.Pipeline, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("app %q: %w", name, ErrNotFound)
	}
	a.CreatedAt = time.Unix(created, 0)
	return a, err
}

func (s *Store) ListApps() ([]App, error) {
	rows, err := s.db.Query(`SELECT id, name, pipeline, created_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		var created int64
		if err := rows.Scan(&a.ID, &a.Name, &a.Pipeline, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteApp(id int64) error {
	_, err := s.db.Exec(`DELETE FROM apps WHERE id = ?`, id)
	return err
}

func (s *Store) SetPipeline(appID int64, on bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE apps SET pipeline = ? WHERE id = ?`, on, appID); err != nil {
		return err
	}
	if on {
		_, err = tx.Exec(`INSERT OR IGNORE INTO stages(app_id, name) VALUES(?, ?)`, appID, api.StagingStage)
	} else {
		_, err = tx.Exec(`DELETE FROM stages WHERE app_id = ? AND name = ?`, appID, api.StagingStage)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

const stageCols = `s.id, s.app_id, a.name, s.name, s.active_color, s.current_release, s.config_version`

func scanStage(sc interface{ Scan(...any) error }) (Stage, error) {
	var st Stage
	err := sc.Scan(&st.ID, &st.AppID, &st.App, &st.Name, &st.ActiveColor, &st.CurrentRelease, &st.ConfigVersion)
	return st, err
}

func (s *Store) GetStage(app, stage string) (Stage, error) {
	row := s.db.QueryRow(`SELECT `+stageCols+` FROM stages s JOIN apps a ON a.id = s.app_id WHERE a.name = ? AND s.name = ?`, app, stage)
	st, err := scanStage(row)
	if errors.Is(err, sql.ErrNoRows) {
		if _, aerr := s.GetApp(app); aerr != nil {
			return st, aerr
		}
		return st, fmt.Errorf("app %q has no %s stage%s: %w", app, stage, pipelineHint(stage), ErrNotFound)
	}
	return st, err
}

func pipelineHint(stage string) string {
	if stage == api.StagingStage {
		return " (enable it with `dokwalt pipeline:enable`)"
	}
	return ""
}

func (s *Store) GetStageByID(id int64) (Stage, error) {
	row := s.db.QueryRow(`SELECT `+stageCols+` FROM stages s JOIN apps a ON a.id = s.app_id WHERE s.id = ?`, id)
	return scanStage(row)
}

// Stages lists stages of one app, or of all apps when appID is 0.
// Production comes before staging.
func (s *Store) Stages(appID int64) ([]Stage, error) {
	q := `SELECT ` + stageCols + ` FROM stages s JOIN apps a ON a.id = s.app_id`
	var args []any
	if appID != 0 {
		q += ` WHERE s.app_id = ?`
		args = append(args, appID)
	}
	q += ` ORDER BY a.name, CASE s.name WHEN 'production' THEN 0 ELSE 1 END`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stage
	for rows.Next() {
		st, err := scanStage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) SetStageActive(stageID int64, color string, release int) error {
	_, err := s.db.Exec(`UPDATE stages SET active_color = ?, current_release = ? WHERE id = ?`, color, release, stageID)
	return err
}

// ---- releases ----

type Release struct {
	ID            int64
	StageID       int64
	Version       int
	Status        string
	Images        map[string]api.Image
	Compose       map[string]any
	ConfigVersion int
	Color         string
	Description   string
	GitSHA        string
	CreatedAt     time.Time
	FinishedAt    *time.Time
	Error         string
}

const (
	StatusPending    = "pending"
	StatusDeploying  = "deploying"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusSuperseded = "superseded"
)

func (s *Store) CreateRelease(r *Release) error {
	images, _ := json.Marshal(r.Images)
	compose, _ := json.Marshal(r.Compose)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var next int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) + 1 FROM releases WHERE stage_id = ?`, r.StageID).Scan(&next); err != nil {
		return err
	}
	r.Version = next
	r.CreatedAt = time.Now()
	res, err := tx.Exec(`INSERT INTO releases(stage_id, version, status, images, compose, config_version, color, description, git_sha, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, r.StageID, r.Version, r.Status, string(images), string(compose), r.ConfigVersion, r.Color, r.Description, r.GitSHA, r.CreatedAt.Unix())
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return tx.Commit()
}

func (s *Store) FinishRelease(id int64, status, errMsg string) error {
	_, err := s.db.Exec(`UPDATE releases SET status = ?, error = ?, finished_at = ? WHERE id = ?`, status, errMsg, time.Now().Unix(), id)
	return err
}

func (s *Store) SetReleaseStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE releases SET status = ? WHERE id = ?`, status, id)
	return err
}

// SupersedeOthers marks previous succeeded releases of a stage as superseded.
func (s *Store) SupersedeOthers(stageID int64, keep int) error {
	_, err := s.db.Exec(`UPDATE releases SET status = ? WHERE stage_id = ? AND status = ? AND version != ?`, StatusSuperseded, stageID, StatusSucceeded, keep)
	return err
}

const releaseCols = `id, stage_id, version, status, images, compose, config_version, color, description, git_sha, created_at, finished_at, error`

func scanRelease(sc interface{ Scan(...any) error }) (Release, error) {
	var r Release
	var images, compose string
	var created int64
	var finished sql.NullInt64
	err := sc.Scan(&r.ID, &r.StageID, &r.Version, &r.Status, &images, &compose, &r.ConfigVersion, &r.Color, &r.Description, &r.GitSHA, &created, &finished, &r.Error)
	if err != nil {
		return r, err
	}
	_ = json.Unmarshal([]byte(images), &r.Images)
	_ = json.Unmarshal([]byte(compose), &r.Compose)
	r.CreatedAt = time.Unix(created, 0)
	if finished.Valid {
		t := time.Unix(finished.Int64, 0)
		r.FinishedAt = &t
	}
	return r, nil
}

func (s *Store) GetRelease(stageID int64, version int) (Release, error) {
	row := s.db.QueryRow(`SELECT `+releaseCols+` FROM releases WHERE stage_id = ? AND version = ?`, stageID, version)
	r, err := scanRelease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("release v%d: %w", version, ErrNotFound)
	}
	return r, err
}

func (s *Store) Releases(stageID int64, limit int) ([]Release, error) {
	rows, err := s.db.Query(`SELECT `+releaseCols+` FROM releases WHERE stage_id = ? ORDER BY version DESC LIMIT ?`, stageID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReleasesWithStatus returns releases of any stage in the given status.
func (s *Store) ReleasesWithStatus(status string) ([]Release, error) {
	rows, err := s.db.Query(`SELECT `+releaseCols+` FROM releases WHERE status = ?`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- config ----

// Config returns the decrypted config of a stage.
func (s *Store) Config(stageID int64) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM config WHERE stage_id = ?`, stageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		plain, err := s.box.Open(v)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", k, err)
		}
		out[k] = string(plain)
	}
	return out, rows.Err()
}

// UpdateConfig applies changes, snapshots the result as a new config
// version and returns that version.
func (s *Store) UpdateConfig(stageID int64, set map[string]string, unset []string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for k, v := range set {
		sealed, err := s.box.Seal([]byte(v))
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO config(stage_id, key, value) VALUES(?,?,?) ON CONFLICT(stage_id, key) DO UPDATE SET value = excluded.value`, stageID, k, sealed); err != nil {
			return 0, err
		}
	}
	for _, k := range unset {
		if _, err := tx.Exec(`DELETE FROM config WHERE stage_id = ? AND key = ?`, stageID, k); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return s.snapshotConfig(stageID)
}

// ReplaceConfig replaces the whole config (rollback --with-config, import).
func (s *Store) ReplaceConfig(stageID int64, cfg map[string]string) (int, error) {
	if _, err := s.db.Exec(`DELETE FROM config WHERE stage_id = ?`, stageID); err != nil {
		return 0, err
	}
	return s.UpdateConfig(stageID, cfg, nil)
}

func (s *Store) snapshotConfig(stageID int64) (int, error) {
	cfg, err := s.Config(stageID)
	if err != nil {
		return 0, err
	}
	b, _ := json.Marshal(cfg)
	sealed, err := s.box.Seal(b)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var next int
	if err := tx.QueryRow(`SELECT config_version + 1 FROM stages WHERE id = ?`, stageID).Scan(&next); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO config_versions(stage_id, version, snapshot, created_at) VALUES(?,?,?,?)`, stageID, next, sealed, time.Now().Unix()); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE stages SET config_version = ? WHERE id = ?`, next, stageID); err != nil {
		return 0, err
	}
	return next, tx.Commit()
}

func (s *Store) ConfigSnapshot(stageID int64, version int) (map[string]string, error) {
	if version == 0 {
		return map[string]string{}, nil
	}
	var sealed []byte
	err := s.db.QueryRow(`SELECT snapshot FROM config_versions WHERE stage_id = ? AND version = ?`, stageID, version).Scan(&sealed)
	if err != nil {
		return nil, err
	}
	plain, err := s.box.Open(sealed)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	return out, json.Unmarshal(plain, &out)
}

// ---- domains ----

type Domain struct {
	ID         int64
	StageID    int64
	Hostname   string
	Service    string
	Port       int
	RedirectTo string
}

func (s *Store) AddDomain(d Domain) error {
	_, err := s.db.Exec(`INSERT INTO domains(stage_id, hostname, service, port, redirect_to) VALUES(?,?,?,?,?)`, d.StageID, d.Hostname, d.Service, d.Port, d.RedirectTo)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("domain %s is already attached", d.Hostname)
	}
	return err
}

func (s *Store) RemoveDomain(stageID int64, hostname string) error {
	res, err := s.db.Exec(`DELETE FROM domains WHERE stage_id = ? AND hostname = ?`, stageID, hostname)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("domain %s: %w", hostname, ErrNotFound)
	}
	return nil
}

// Domains lists domains for one stage, or all when stageID is 0.
func (s *Store) Domains(stageID int64) ([]Domain, error) {
	q := `SELECT id, stage_id, hostname, service, port, redirect_to FROM domains`
	var args []any
	if stageID != 0 {
		q += ` WHERE stage_id = ?`
		args = append(args, stageID)
	}
	q += ` ORDER BY hostname`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.ID, &d.StageID, &d.Hostname, &d.Service, &d.Port, &d.RedirectTo); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- service overrides ----

type Override struct {
	Service       string
	Stateful      *bool
	HealthPath    string
	HealthTimeout int
}

func (s *Store) Overrides(stageID int64) (map[string]Override, error) {
	rows, err := s.db.Query(`SELECT service, stateful, health_path, health_timeout_s FROM service_overrides WHERE stage_id = ?`, stageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Override{}
	for rows.Next() {
		var o Override
		var sf sql.NullBool
		if err := rows.Scan(&o.Service, &sf, &o.HealthPath, &o.HealthTimeout); err != nil {
			return nil, err
		}
		if sf.Valid {
			v := sf.Bool
			o.Stateful = &v
		}
		out[o.Service] = o
	}
	return out, rows.Err()
}

func (s *Store) SetOverride(stageID int64, o Override) error {
	var sf any
	if o.Stateful != nil {
		sf = *o.Stateful
	}
	_, err := s.db.Exec(`INSERT INTO service_overrides(stage_id, service, stateful, health_path, health_timeout_s) VALUES(?,?,?,?,?)
		ON CONFLICT(stage_id, service) DO UPDATE SET stateful = excluded.stateful, health_path = excluded.health_path, health_timeout_s = excluded.health_timeout_s`,
		stageID, o.Service, sf, o.HealthPath, o.HealthTimeout)
	return err
}

// ---- settings ----

func (s *Store) Setting(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SettingsWithPrefix returns all settings whose key starts with prefix.
func (s *Store) SettingsWithPrefix(prefix string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings WHERE key LIKE ? || '%'`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---- alerts ----

func (s *Store) AlertChannels() ([]api.AlertChannel, error) {
	rows, err := s.db.Query(`SELECT id, kind, url FROM alert_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.AlertChannel
	for rows.Next() {
		var c api.AlertChannel
		if err := rows.Scan(&c.ID, &c.Kind, &c.URL); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AddAlertChannel(kind, url string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO alert_channels(kind, url) VALUES(?, ?)`, kind, url)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) RemoveAlertChannel(id int) error {
	res, err := s.db.Exec(`DELETE FROM alert_channels WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("alert channel %d: %w", id, ErrNotFound)
	}
	return nil
}

type AlertState struct {
	Firing   bool
	LastSent time.Time
}

func (s *Store) AlertStates() (map[string]AlertState, error) {
	rows, err := s.db.Query(`SELECT key, firing, last_sent FROM alert_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AlertState{}
	for rows.Next() {
		var k string
		var st AlertState
		var ts int64
		if err := rows.Scan(&k, &st.Firing, &ts); err != nil {
			return nil, err
		}
		st.LastSent = time.Unix(ts, 0)
		out[k] = st
	}
	return out, rows.Err()
}

func (s *Store) SetAlertState(key string, st AlertState) error {
	if !st.Firing && st.LastSent.IsZero() {
		_, err := s.db.Exec(`DELETE FROM alert_state WHERE key = ?`, key)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO alert_state(key, firing, last_sent) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET firing = excluded.firing, last_sent = excluded.last_sent`, key, st.Firing, st.LastSent.Unix())
	return err
}

// ---- metrics ----

type ContainerSample struct {
	TS       int64
	StageID  int64
	Service  string
	CPUPct   float64
	MemBytes uint64
	NetRx    uint64
	NetTx    uint64
	BlkR     uint64
	BlkW     uint64
}

type HTTPSample struct {
	TS                       int64
	Hostname                 string
	Requests, S2, S3, S4, S5 uint64
	P50, P95, P99            float64
}

type HostSample struct {
	TS                  int64
	CPUPct              float64
	MemUsed, MemTotal   uint64
	DiskUsed, DiskTotal uint64
	Load1, TempC        float64
}

func (s *Store) InsertMetrics(cs []ContainerSample, hs []HTTPSample, host *HostSample) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range cs {
		if _, err := tx.Exec(`INSERT INTO metrics_containers VALUES(?,?,?,?,?,?,?,?,?)`, c.TS, c.StageID, c.Service, c.CPUPct, c.MemBytes, c.NetRx, c.NetTx, c.BlkR, c.BlkW); err != nil {
			return err
		}
	}
	for _, h := range hs {
		if _, err := tx.Exec(`INSERT INTO metrics_http VALUES(?,?,?,?,?,?,?,?,?,?)`, h.TS, h.Hostname, h.Requests, h.S2, h.S3, h.S4, h.S5, h.P50, h.P95, h.P99); err != nil {
			return err
		}
	}
	if host != nil {
		if _, err := tx.Exec(`INSERT INTO metrics_host VALUES(?,?,?,?,?,?,?,?)`, host.TS, host.CPUPct, host.MemUsed, host.MemTotal, host.DiskUsed, host.DiskTotal, host.Load1, host.TempC); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) PruneMetrics(before int64) error {
	for _, t := range []string{"metrics_containers", "metrics_http", "metrics_host"} {
		if _, err := s.db.Exec(`DELETE FROM `+t+` WHERE ts < ?`, before); err != nil {
			return err
		}
	}
	return nil
}

// MetricsHistory returns points bucketed to `step` seconds since `since`.
func (s *Store) MetricsHistory(stageIDs []int64, hostnames []string, since, step int64) (api.MetricsHistory, error) {
	out := api.MetricsHistory{Services: map[string][]api.MetricsPoint{}, HTTP: map[string][]api.MetricsPoint{}}
	if len(stageIDs) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(stageIDs)), ",")
		args := []any{step, step}
		for _, id := range stageIDs {
			args = append(args, id)
		}
		args = append(args, since)
		rows, err := s.db.Query(`SELECT (m.ts / ?) * ? AS b, st.name, m.service, AVG(m.cpu_pct), AVG(m.mem_bytes)
			FROM metrics_containers m JOIN stages st ON st.id = m.stage_id
			WHERE m.stage_id IN (`+ph+`) AND m.ts >= ? GROUP BY b, m.stage_id, m.service ORDER BY b`, args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var b int64
			var stage, svc string
			var cpu, mem float64
			if err := rows.Scan(&b, &stage, &svc, &cpu, &mem); err != nil {
				rows.Close()
				return out, err
			}
			key := svc
			if len(stageIDs) > 1 {
				key = stage + "/" + svc
			}
			out.Services[key] = append(out.Services[key], api.MetricsPoint{TS: b, CPUPct: cpu, MemBytes: uint64(mem)})
		}
		rows.Close()
	}
	for _, h := range hostnames {
		rows, err := s.db.Query(`SELECT (ts / ?) * ? AS b, SUM(requests), SUM(s5xx), MAX(p95_ms) FROM metrics_http WHERE hostname = ? AND ts >= ? GROUP BY b ORDER BY b`, step, step, h, since)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var p api.MetricsPoint
			var p95 sql.NullFloat64
			if err := rows.Scan(&p.TS, &p.Requests, &p.S5xx, &p95); err != nil {
				rows.Close()
				return out, err
			}
			p.P95 = p95.Float64
			out.HTTP[h] = append(out.HTTP[h], p)
		}
		rows.Close()
	}
	rows, err := s.db.Query(`SELECT (ts / ?) * ? AS b, AVG(cpu_pct), AVG(mem_used) FROM metrics_host WHERE ts >= ? GROUP BY b ORDER BY b`, step, step, since)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var p api.MetricsPoint
		var mem float64
		if err := rows.Scan(&p.TS, &p.CPUPct, &mem); err != nil {
			return out, err
		}
		p.MemBytes = uint64(mem)
		out.Host = append(out.Host, p)
	}
	return out, rows.Err()
}

// SortedKeys is a small helper used by callers rendering maps.
func SortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
