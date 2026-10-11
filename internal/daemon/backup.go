package daemon

// Off-site backups: every night (backup_time, server local time), dump every
// database of every Postgres service plus DokWalt's own state, upload them to
// S3-compatible storage (Cloudflare R2…), then prune old backups. Layout and
// retention rules live in internal/backup; this file runs them.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/backup"
	"github.com/ddahan/dokwalt/internal/s3"
	"github.com/ddahan/dokwalt/internal/ui"
)

const (
	backupDefaultTime   = "03:00"
	backupDefaultDaily  = 7
	backupDefaultWeekly = 4
	backupTestObject    = ".dokwalt-test"
	backupAlertKey      = "backup"
	backupAlertTitle    = "Backup failed"
	// Interrupted backups (no manifest) younger than this may still be
	// running elsewhere (a second server with the same prefix): left alone.
	backupStaleAfter = 24 * time.Hour
)

var errNoBackups = &httpError{status: http.StatusBadRequest, msg: "backups aren't set up", hint: "run `dokwalt backup:setup`"}

// backupHTTP has timeouts on every phase but the body, which can take long
// for a big dump on a slow uplink; the request context bounds it.
var backupHTTP = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 2 * time.Minute,
	IdleConnTimeout:       60 * time.Second,
}}

// backupStorage returns the configured bucket and folder.
func (d *Daemon) backupStorage() (*s3.Client, string, error) {
	endpoint := d.store.Setting("backup_endpoint", "")
	if endpoint == "" {
		return nil, "", errNoBackups
	}
	secret, err := d.store.SecretSetting("backup_secret_key")
	if err != nil {
		return nil, "", err
	}
	c := &s3.Client{
		Endpoint: endpoint, Region: d.store.Setting("backup_region", "auto"), Bucket: d.store.Setting("backup_bucket", ""),
		AccessKey: d.store.Setting("backup_access_key", ""), SecretKey: secret, HTTP: backupHTTP,
	}
	return c, d.store.Setting("backup_prefix", ""), nil
}

func (d *Daemon) backupKeep() (daily, weekly int) {
	daily, _ = strconv.Atoi(d.store.Setting("backup_keep_daily", strconv.Itoa(backupDefaultDaily)))
	weekly, _ = strconv.Atoi(d.store.Setting("backup_keep_weekly", strconv.Itoa(backupDefaultWeekly)))
	return max(daily, 1), max(weekly, 0)
}

func (d *Daemon) backupStatus() api.BackupStatus {
	s := api.BackupStatus{Running: d.backupRunning.Load()}
	if raw := d.store.Setting("backup_last", ""); raw != "" {
		var run api.BackupRun
		if json.Unmarshal([]byte(raw), &run) == nil {
			s.Last = &run
		}
	}
	if d.store.Setting("backup_endpoint", "") == "" {
		return s
	}
	s.Configured = true
	s.Endpoint = d.store.Setting("backup_endpoint", "")
	s.Region = d.store.Setting("backup_region", "auto")
	s.Bucket = d.store.Setting("backup_bucket", "")
	s.Prefix = d.store.Setting("backup_prefix", "")
	s.AccessKey = d.store.Setting("backup_access_key", "")
	s.Time = d.store.Setting("backup_time", backupDefaultTime)
	s.TimeZone = time.Now().Format("MST")
	s.KeepDaily, s.KeepWeekly = d.backupKeep()
	s.Next = backup.Next(time.Now(), s.Time, d.store.Setting("backup_last_day", ""))
	return s
}

// backupLoop starts the nightly backup when it's due.
func (d *Daemon) backupLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if d.store.Setting("backup_endpoint", "") == "" || d.backupRunning.Load() {
			continue
		}
		now := time.Now()
		if !backup.Due(now, d.store.Setting("backup_time", backupDefaultTime), d.store.Setting("backup_last_day", "")) {
			continue
		}
		// One attempt per day: a failure alerts instead of retrying every minute.
		_ = d.store.SetSetting("backup_last_day", now.Format("2006-01-02"))
		_, _ = d.runBackup(ctx, "schedule", func(e api.Event) {
			if e.Status == "warn" || e.Status == "error" {
				d.log.Warn("backup", "step", e.Step, "msg", e.Message)
			}
		})
	}
}

// pgTarget is one running Postgres service to back up.
type pgTarget struct {
	App, Stage, Service string
	Container           string
	User, MaintenanceDB string
}

func (t pgTarget) dir() string { return backup.ServiceDir(t.App, t.Stage, t.Service) }

// postgresTargets finds every Postgres service of every deployed stage.
// Services that should run but don't are returned as problems; stopped
// stages (`dokwalt stop`) are skipped on purpose.
func (d *Daemon) postgresTargets(ctx context.Context) (targets []pgTarget, problems, skipped []string, err error) {
	stages, err := d.store.Stages(0)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, st := range stages {
		plan, err := d.currentPlan(st)
		if err != nil || plan == nil {
			continue
		}
		var svcs []string
		for _, s := range plan.Services {
			if s.Stateful && dbKind(s.Image) == "postgres" {
				svcs = append(svcs, s.Name)
			}
		}
		if len(svcs) == 0 {
			continue
		}
		if d.engine.Stopped(st.ID) {
			skipped = append(skipped, st.Key()+" (stopped)")
			continue
		}
		containers, err := d.engine.StageContainers(ctx, st)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, name := range svcs {
			cs := containers[name]
			if len(cs) == 0 || cs[0].State != "running" {
				problems = append(problems, fmt.Sprintf("%s: service %s is not running", st.Key(), name))
				continue
			}
			ci, err := d.docker.Inspect(ctx, cs[0].ID)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s: %v", st.Key(), name, err))
				continue
			}
			env := ci.EnvMap()
			user := firstSet(env["POSTGRES_USER"], "postgres")
			targets = append(targets, pgTarget{App: st.App, Stage: st.Name, Service: name, Container: cs[0].Name,
				User: user, MaintenanceDB: firstSet(env["POSTGRES_DB"], user)})
		}
	}
	return targets, problems, skipped, nil
}

// dockerOut runs `docker <args>` and writes its stdout to w. Errors carry
// the last lines of stderr.
func dockerOut(ctx context.Context, w io.Writer, stdin io.Reader, args ...string) error {
	var errb tailBuffer
	c := exec.CommandContext(ctx, "docker", args...)
	c.Stdout, c.Stderr, c.Stdin = w, &errb, stdin
	if err := c.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	return nil
}

// tailBuffer keeps the last 2 KB written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

func (t pgTarget) databases(ctx context.Context) ([]string, error) {
	var out bytes.Buffer
	err := dockerOut(ctx, &out, nil, "exec", t.Container, "psql", "-XAt", "-U", t.User, "-d", t.MaintenanceDB,
		"-c", "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY 1")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out.String()), nil
}

// toFile runs `docker <args>` into a new file.
func toFile(ctx context.Context, path string, args ...string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = dockerOut(ctx, f, nil, args...)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// runBackup makes one backup. Database problems make it partial (and skip
// pruning, so a run of bad nights never deletes the good backups); storage
// problems fail it.
func (d *Daemon) runBackup(ctx context.Context, trigger string, emit func(api.Event)) (m backup.Manifest, err error) {
	if !d.backupMu.TryLock() {
		return m, &httpError{status: http.StatusConflict, msg: "a backup is already running"}
	}
	defer d.backupMu.Unlock()
	d.backupRunning.Store(true)
	defer d.backupRunning.Store(false)

	start := time.Now()
	m = backup.Manifest{ID: start.UTC().Format(backup.IDFormat), Started: start, Build: d.opts.Build, Trigger: trigger}
	m.Host, _ = os.Hostname()
	defer func() { d.recordBackup(m, start, err) }()

	c, prefix, err := d.backupStorage()
	if err != nil {
		return m, err
	}
	tmp := filepath.Join(d.opts.DataDir, "backup-tmp")
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return m, err
	}
	defer os.RemoveAll(tmp)

	// upload sends a local file, records it in the manifest and deletes it,
	// so the server never holds more than one dump at a time.
	upload := func(local string, f backup.File) error {
		defer os.Remove(local)
		sum, size, err := fileSHA256(local)
		if err != nil {
			return err
		}
		f.SHA256, f.Size = sum, size
		if err := c.PutFile(ctx, backup.Key(prefix, m.ID, f.Path), local); err != nil {
			return fmt.Errorf("upload %s: %w", f.Path, err)
		}
		m.Files = append(m.Files, f)
		return nil
	}

	emit(api.Event{Step: "dokwalt", Status: "start", Message: "DokWalt state (dokwalt.db)"})
	local := filepath.Join(tmp, "dokwalt.db")
	if err := d.store.CopyTo(local); err != nil {
		return m, fmt.Errorf("copy dokwalt.db: %w", err)
	}
	if err := upload(local, backup.File{Path: "dokwalt.db", Kind: backup.KindDokWalt}); err != nil {
		return m, err
	}
	emit(api.Event{Step: "dokwalt", Status: "done", Message: "DokWalt state (dokwalt.db) " + humanSize(m.Files[0].Size)})

	targets, problems, skipped, err := d.postgresTargets(ctx)
	if err != nil {
		return m, err
	}
	m.Errors = append(m.Errors, problems...)
	for _, p := range problems {
		emit(api.Event{Step: "targets", Status: "warn", Message: p})
	}
	for _, s := range skipped {
		emit(api.Event{Step: "targets", Status: "warn", Message: "Skipped " + s})
	}
	for _, t := range targets {
		base := backup.File{Kind: backup.KindGlobals, App: t.App, Stage: t.Stage, Service: t.Service}
		step := t.dir()
		emit(api.Event{Step: step, Status: "start", Message: "Roles of " + step})
		local := filepath.Join(tmp, "globals.sql")
		if err := toFile(ctx, local, "exec", t.Container, "pg_dumpall", "--globals-only", "-U", t.User); err != nil {
			m.Errors = append(m.Errors, fmt.Sprintf("%s: roles: %v", step, err))
			emit(api.Event{Step: step, Status: "warn", Message: fmt.Sprintf("Roles of %s: %v", step, err)})
		} else {
			base.Path = t.dir() + "/globals.sql"
			if err := upload(local, base); err != nil {
				return m, err
			}
			emit(api.Event{Step: step, Status: "done", Message: "Roles of " + step})
		}

		dbs, err := t.databases(ctx)
		if err != nil {
			m.Errors = append(m.Errors, fmt.Sprintf("%s: list databases: %v", step, err))
			emit(api.Event{Step: step, Status: "warn", Message: fmt.Sprintf("%s: list databases: %v", step, err)})
			continue
		}
		for _, db := range dbs {
			name := step + "/" + db + ".dump"
			emit(api.Event{Step: name, Status: "start", Message: "Database " + db + " (" + step + ")"})
			local := filepath.Join(tmp, "db.dump")
			err := toFile(ctx, local, "exec", t.Container, "pg_dump", "--format=custom", "-U", t.User, "-d", db)
			if err == nil {
				err = checkDumpFile(local)
			}
			if err != nil {
				_ = os.Remove(local)
				m.Errors = append(m.Errors, fmt.Sprintf("%s: database %s: %v", step, db, err))
				emit(api.Event{Step: name, Status: "warn", Message: fmt.Sprintf("Database %s (%s): %v", db, step, err)})
				continue
			}
			f := base
			f.Kind, f.Database, f.Path = backup.KindPostgres, db, name
			if err := upload(local, f); err != nil {
				return m, err
			}
			emit(api.Event{Step: name, Status: "done", Message: fmt.Sprintf("Database %s (%s) %s", db, step, humanSize(m.Files[len(m.Files)-1].Size))})
		}
	}

	m.DurationMS = time.Since(start).Milliseconds()
	body, _ := json.MarshalIndent(m, "", "  ")
	if err := c.PutBytes(ctx, backup.Key(prefix, m.ID, backup.ManifestName), body); err != nil {
		return m, fmt.Errorf("upload manifest: %w", err)
	}

	if len(m.Errors) == 0 {
		emit(api.Event{Step: "prune", Status: "start", Message: "Removing old backups"})
		n, err := d.pruneBackups(ctx, c, prefix, m.ID)
		if err != nil {
			emit(api.Event{Step: "prune", Status: "warn", Message: "Removing old backups: " + err.Error()})
		} else {
			emit(api.Event{Step: "prune", Status: "done", Message: fmt.Sprintf("Removed %d old backup(s)", n)})
		}
	} else {
		emit(api.Event{Step: "prune", Status: "warn", Message: "Old backups kept: this backup is incomplete"})
	}
	return m, nil
}

func checkDumpFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 5)
	if _, err := io.ReadFull(f, head); err != nil || string(head) != "PGDMP" {
		return errors.New("pg_dump produced an invalid archive")
	}
	return nil
}

// recordBackup stores the outcome of a run and alerts on failure.
func (d *Daemon) recordBackup(m backup.Manifest, start time.Time, err error) {
	run := api.BackupRun{ID: m.ID, Started: start, Duration: time.Since(start).Milliseconds(), Size: m.Size(), Files: len(m.Files), Status: "ok"}
	switch {
	case err != nil:
		var he *httpError
		if errors.As(err, &he) && he.status == http.StatusConflict {
			return // another run is in progress: nothing happened
		}
		run.Status, run.Error, run.ID = "failed", err.Error(), ""
	case len(m.Errors) > 0:
		run.Status, run.Error = "partial", strings.Join(m.Errors, "\n")
	}
	if b, jerr := json.Marshal(run); jerr == nil {
		_ = d.store.SetSetting("backup_last", string(b))
	}
	if run.Status == "ok" {
		d.alerts.Fire(backupAlertKey, backupAlertTitle, "Backup "+m.ID+" completed ("+humanSize(run.Size)+")", false)
		d.log.Info("backup completed", "id", m.ID, "size", run.Size, "files", run.Files)
		return
	}
	d.log.Error("backup", "status", run.Status, "err", run.Error)
	d.alerts.Fire(backupAlertKey, backupAlertTitle, "The off-site backup is "+run.Status+":\n"+run.Error+
		"\nRun `dokwalt backups` for details, `dokwalt backup:now` to retry.", true)
}

// backupFolders lists the objects of each backup folder under prefix.
func backupFolders(ctx context.Context, c *s3.Client, prefix string) (map[string][]s3.Object, error) {
	objs, err := c.List(ctx, strings.TrimSuffix(prefix, "/")+"/")
	if err != nil {
		return nil, err
	}
	out := map[string][]s3.Object{}
	for _, o := range objs {
		if id, _, ok := backup.SplitKey(prefix, o.Key); ok {
			out[id] = append(out[id], o)
		}
	}
	return out, nil
}

func hasManifest(prefix, id string, objs []s3.Object) bool {
	key := backup.Key(prefix, id, backup.ManifestName)
	for _, o := range objs {
		if o.Key == key {
			return true
		}
	}
	return false
}

// pruneBackups deletes complete backups that retention doesn't keep, and
// interrupted ones older than a day. It returns how many it deleted.
func (d *Daemon) pruneBackups(ctx context.Context, c *s3.Client, prefix, current string) (int, error) {
	folders, err := backupFolders(ctx, c, prefix)
	if err != nil {
		return 0, err
	}
	var complete []string
	var drop []string
	for id, objs := range folders {
		if hasManifest(prefix, id, objs) {
			complete = append(complete, id)
		} else if t, _ := backup.ParseID(id); id != current && time.Since(t) > backupStaleAfter {
			drop = append(drop, id)
		}
	}
	daily, weekly := d.backupKeep()
	keep := backup.Keep(complete, daily, weekly, time.Local)
	keep[current] = true
	for _, id := range complete {
		if !keep[id] {
			drop = append(drop, id)
		}
	}
	sort.Strings(drop)
	for _, id := range drop {
		// The manifest goes first: a half-deleted backup then reads as
		// interrupted, and the next prune finishes the job.
		objs := folders[id]
		sort.Slice(objs, func(i, j int) bool { return strings.HasSuffix(objs[i].Key, "/"+backup.ManifestName) })
		for _, o := range objs {
			if err := c.Delete(ctx, o.Key); err != nil {
				return 0, err
			}
		}
	}
	return len(drop), nil
}

// manifests reads the manifests of complete backups, newest first.
func (d *Daemon) manifests(ctx context.Context, c *s3.Client, prefix string) ([]backup.Manifest, error) {
	folders, err := backupFolders(ctx, c, prefix)
	if err != nil {
		return nil, err
	}
	out := []backup.Manifest{}
	for id, objs := range folders {
		if !hasManifest(prefix, id, objs) {
			continue
		}
		b, err := c.GetBytes(ctx, backup.Key(prefix, id, backup.ManifestName))
		if err != nil {
			return nil, err
		}
		var m backup.Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("backup %s: unreadable manifest: %w", id, err)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// manifest reads one backup's manifest; "latest" is the newest backup.
func (d *Daemon) manifest(ctx context.Context, c *s3.Client, prefix, id string) (backup.Manifest, error) {
	if id == "latest" {
		all, err := d.manifests(ctx, c, prefix)
		if err != nil {
			return backup.Manifest{}, err
		}
		if len(all) == 0 {
			return backup.Manifest{}, &httpError{status: http.StatusNotFound, msg: "no backups yet", hint: "run `dokwalt backup:now`"}
		}
		return all[0], nil
	}
	if _, err := backup.ParseID(id); err != nil {
		return backup.Manifest{}, badRequest("%v", err)
	}
	b, err := c.GetBytes(ctx, backup.Key(prefix, id, backup.ManifestName))
	if s3.IsNotFound(err) {
		return backup.Manifest{}, &httpError{status: http.StatusNotFound, msg: "backup " + id + " not found", hint: "list them with `dokwalt backups`"}
	}
	if err != nil {
		return backup.Manifest{}, err
	}
	var m backup.Manifest
	return m, json.Unmarshal(b, &m)
}

// ---- handlers ----

func (d *Daemon) getBackupConfig(w http.ResponseWriter, r *http.Request) error {
	return writeJSON(w, d.backupStatus())
}

// setBackupConfig validates the settings by writing, listing and deleting a
// test object before saving them.
func (d *Daemon) setBackupConfig(w http.ResponseWriter, r *http.Request) error {
	var in api.BackupConfig
	if err := readJSON(r, &in); err != nil {
		return err
	}
	pick := func(v, key, def string) string {
		if v != "" {
			return v
		}
		return d.store.Setting(key, def)
	}
	cfg := api.BackupConfig{
		Endpoint: strings.TrimRight(pick(in.Endpoint, "backup_endpoint", ""), "/"), Region: pick(in.Region, "backup_region", "auto"),
		Bucket: pick(in.Bucket, "backup_bucket", ""), Prefix: strings.Trim(pick(in.Prefix, "backup_prefix", ""), "/"),
		AccessKey: pick(in.AccessKey, "backup_access_key", ""), SecretKey: in.SecretKey, Time: pick(in.Time, "backup_time", backupDefaultTime),
		KeepDaily: in.KeepDaily, KeepWeekly: in.KeepWeekly,
	}
	if cfg.SecretKey == "" {
		cfg.SecretKey, _ = d.store.SecretSetting("backup_secret_key")
	}
	if cfg.Prefix == "" {
		host, _ := os.Hostname()
		cfg.Prefix = "dokwalt/" + firstSet(host, "server")
	}
	if cfg.KeepDaily == 0 {
		cfg.KeepDaily, _ = strconv.Atoi(d.store.Setting("backup_keep_daily", strconv.Itoa(backupDefaultDaily)))
	}
	if cfg.KeepWeekly == 0 {
		cfg.KeepWeekly, _ = strconv.Atoi(d.store.Setting("backup_keep_weekly", strconv.Itoa(backupDefaultWeekly)))
	}
	u, err := url.Parse(cfg.Endpoint)
	switch {
	case cfg.Endpoint == "":
		return badRequest("missing endpoint (--r2-account or --endpoint)")
	case err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/"):
		return badRequest("invalid endpoint %q (want https://host, without the bucket)", cfg.Endpoint)
	case cfg.Bucket == "":
		return badRequest("missing --bucket")
	case cfg.AccessKey == "" || cfg.SecretKey == "":
		return badRequest("missing access key or secret key")
	case cfg.KeepDaily < 1 || cfg.KeepWeekly < 1:
		return badRequest("--keep-daily and --keep-weekly must be at least 1")
	}
	if _, _, err := backup.ParseClock(cfg.Time); err != nil {
		return badRequest("%v", err)
	}

	c := &s3.Client{Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket, AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey, HTTP: backupHTTP}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	key := cfg.Prefix + "/" + backupTestObject
	if err := c.PutBytes(ctx, key, []byte("written by dokwalt backup:setup\n")); err != nil {
		return badRequest("can't write to bucket %s: %v", cfg.Bucket, err)
	}
	if _, err := c.List(ctx, cfg.Prefix+"/"); err != nil {
		return badRequest("can't list bucket %s: %v", cfg.Bucket, err)
	}
	if err := c.Delete(ctx, key); err != nil {
		return badRequest("can't delete from bucket %s (needed to remove old backups): %v", cfg.Bucket, err)
	}

	for k, v := range map[string]string{
		"backup_endpoint": cfg.Endpoint, "backup_region": cfg.Region, "backup_bucket": cfg.Bucket, "backup_prefix": cfg.Prefix,
		"backup_access_key": cfg.AccessKey, "backup_time": cfg.Time,
		"backup_keep_daily": strconv.Itoa(cfg.KeepDaily), "backup_keep_weekly": strconv.Itoa(cfg.KeepWeekly),
	} {
		if err := d.store.SetSetting(k, v); err != nil {
			return err
		}
	}
	if err := d.store.SetSecretSetting("backup_secret_key", cfg.SecretKey); err != nil {
		return err
	}
	// Setting up at 15:00 for 03:00 shouldn't start a backup a minute later:
	// the first scheduled one is the next 03:00 (`backup:now` runs one today).
	if now := time.Now(); backup.Due(now, cfg.Time, d.store.Setting("backup_last_day", "")) {
		_ = d.store.SetSetting("backup_last_day", now.Format("2006-01-02"))
	}
	return writeJSON(w, d.backupStatus())
}

func (d *Daemon) deleteBackupConfig(w http.ResponseWriter, r *http.Request) error {
	if err := d.store.DeleteSettings("backup_"); err != nil {
		return err
	}
	d.alerts.Fire(backupAlertKey, backupAlertTitle, "Backups were turned off", false)
	return writeJSON(w, d.backupStatus())
}

func (d *Daemon) postBackupRun(w http.ResponseWriter, r *http.Request) error {
	if _, _, err := d.backupStorage(); err != nil {
		return err
	}
	if d.backupRunning.Load() {
		return &httpError{status: http.StatusConflict, msg: "a backup is already running"}
	}
	s := newStreamer(w)
	ctx, cancel := opCtx()
	defer cancel()
	m, err := d.runBackup(ctx, "manual", s.emit)
	if err == nil && len(m.Errors) > 0 {
		err = fmt.Errorf("backup %s is incomplete:\n%s", m.ID, strings.Join(m.Errors, "\n"))
	}
	return s.finish(err, m.ID, 0)
}

func (d *Daemon) listBackups(w http.ResponseWriter, r *http.Request) error {
	c, prefix, err := d.backupStorage()
	if err != nil {
		return err
	}
	all, err := d.manifests(r.Context(), c, prefix)
	if err != nil {
		return err
	}
	return writeJSON(w, api.BackupList{Status: d.backupStatus(), Backups: all})
}

func (d *Daemon) getBackup(w http.ResponseWriter, r *http.Request) error {
	c, prefix, err := d.backupStorage()
	if err != nil {
		return err
	}
	m, err := d.manifest(r.Context(), c, prefix, r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, m)
}

// getBackupFile streams one file of a backup, so the client never needs the
// bucket's credentials.
func (d *Daemon) getBackupFile(w http.ResponseWriter, r *http.Request) error {
	c, prefix, err := d.backupStorage()
	if err != nil {
		return err
	}
	m, err := d.manifest(r.Context(), c, prefix, r.PathValue("id"))
	if err != nil {
		return err
	}
	f, ok := m.File(r.PathValue("path"))
	if !ok {
		return &httpError{status: http.StatusNotFound, msg: fmt.Sprintf("no file %q in backup %s", r.PathValue("path"), m.ID)}
	}
	body, size, err := c.Get(r.Context(), backup.Key(prefix, m.ID, f.Path))
	if err != nil {
		return err
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Dokwalt-Sha256", f.SHA256)
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	_, _ = io.Copy(w, body) // headers are sent: an error can only cut the stream
	return nil
}

// postBackupRestore downloads a file, checks it against the manifest, and
// restores it into the service it came from.
func (d *Daemon) postBackupRestore(w http.ResponseWriter, r *http.Request) error {
	var in api.BackupRestore
	if err := readJSON(r, &in); err != nil {
		return err
	}
	c, prefix, err := d.backupStorage()
	if err != nil {
		return err
	}
	m, err := d.manifest(r.Context(), c, prefix, r.PathValue("id"))
	if err != nil {
		return err
	}
	f, ok := m.File(in.File)
	if !ok {
		return &httpError{status: http.StatusNotFound, msg: fmt.Sprintf("no file %q in backup %s", in.File, m.ID), hint: "list files with `dokwalt backups " + m.ID + "`"}
	}
	if f.Kind == backup.KindDokWalt {
		return badRequest("dokwalt.db isn't restored from here: download it with `dokwalt backup:download %s dokwalt.db` and follow `dokwalt docs backups`", m.ID)
	}
	st, err := d.store.GetStage(f.App, f.Stage)
	if err != nil {
		return badRequest("%s/%s doesn't exist on this server: deploy it first", f.App, f.Stage)
	}
	containers, err := d.engine.StageContainers(r.Context(), st)
	if err != nil {
		return err
	}
	cs := containers[f.Service]
	if len(cs) == 0 || cs[0].State != "running" {
		return badRequest("%s: service %s is not running", st.Key(), f.Service)
	}
	ci, err := d.docker.Inspect(r.Context(), cs[0].ID)
	if err != nil {
		return err
	}
	env := ci.EnvMap()
	t := pgTarget{App: f.App, Stage: f.Stage, Service: f.Service, Container: cs[0].Name, User: firstSet(env["POSTGRES_USER"], "postgres")}
	t.MaintenanceDB = firstSet(env["POSTGRES_DB"], t.User)

	s := newStreamer(w)
	ctx, cancel := opCtx()
	defer cancel()
	return s.finish(d.restoreFile(ctx, c, prefix, m, f, t, s.emit), f.Path, 0)
}

func (d *Daemon) restoreFile(ctx context.Context, c *s3.Client, prefix string, m backup.Manifest, f backup.File, t pgTarget, emit func(api.Event)) error {
	emit(api.Event{Step: "download", Status: "start", Message: "Downloading " + f.Path})
	tmp, err := os.CreateTemp(d.opts.DataDir, "restore-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	body, _, err := c.Get(ctx, backup.Key(prefix, m.ID, f.Path))
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), body)
	body.Close()
	if err != nil {
		return fmt.Errorf("download %s: %w", f.Path, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return fmt.Errorf("%s doesn't match its checksum: the backup is damaged", f.Path)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	emit(api.Event{Step: "download", Status: "done", Message: "Downloaded " + f.Path + " " + humanSize(f.Size) + " (checksum OK)"})

	switch f.Kind {
	case backup.KindGlobals:
		// Roles that already exist fail with "already exists" and the rest
		// goes on (no ON_ERROR_STOP): the script only adds what's missing.
		emit(api.Event{Step: "restore", Status: "start", Message: "Restoring roles into " + t.dir()})
		var out tailBuffer
		if err := dockerOut(ctx, &out, tmp, "exec", "-i", t.Container, "psql", "-X", "-q", "-U", t.User, "-d", t.MaintenanceDB); err != nil {
			return fmt.Errorf("psql: %w", err)
		}
		emit(api.Event{Step: "restore", Status: "done", Message: "Restored roles into " + t.dir()})
	case backup.KindPostgres:
		var exists bytes.Buffer
		q := "SELECT 1 FROM pg_database WHERE datname = '" + strings.ReplaceAll(f.Database, "'", "''") + "'"
		if err := dockerOut(ctx, &exists, nil, "exec", t.Container, "psql", "-XAt", "-U", t.User, "-d", t.MaintenanceDB, "-c", q); err != nil {
			return err
		}
		// As the server's superuser, without --no-owner: objects get back
		// their original owner (the app's role, which must exist: restore
		// globals.sql first on a fresh server).
		args := []string{"exec", "-i", t.Container, "pg_restore", "-U", t.User}
		if strings.TrimSpace(exists.String()) == "1" {
			args = append(args, "--clean", "--if-exists", "-d", f.Database)
			emit(api.Event{Step: "restore", Status: "start", Message: "Replacing database " + f.Database + " in " + t.dir()})
		} else {
			args = append(args, "--create", "-d", t.MaintenanceDB)
			emit(api.Event{Step: "restore", Status: "start", Message: "Creating database " + f.Database + " in " + t.dir()})
		}
		if err := dockerOut(ctx, io.Discard, tmp, args...); err != nil {
			return fmt.Errorf("pg_restore: %w", err)
		}
		emit(api.Event{Step: "restore", Status: "done", Message: "Restored database " + f.Database + " in " + t.dir()})
	default:
		return badRequest("can't restore %s files", f.Kind)
	}
	return nil
}

func humanSize(n int64) string { return ui.Bytes(uint64(n)) }
