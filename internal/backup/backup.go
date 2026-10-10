// Package backup holds the pure parts of off-site backups: how a backup is
// laid out in the bucket, its manifest, and which backups retention keeps.
// The daemon (internal/daemon/backup.go) does the dumping and uploading.
//
// Bucket layout, one folder per backup:
//
//	<prefix>/<id>/dokwalt.db                       DokWalt's state (config values stay encrypted)
//	<prefix>/<id>/<app>-<stage>-<service>/globals.sql   roles of a Postgres server
//	<prefix>/<id>/<app>-<stage>-<service>/<db>.dump     pg_dump --format=custom
//	<prefix>/<id>/manifest.json                    written last: the backup is complete
//
// A folder without manifest.json is an interrupted backup.
package backup

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// IDFormat names a backup after its start time, in UTC, so IDs sort by date.
const IDFormat = "20060102T150405Z"

const ManifestName = "manifest.json"

// File kinds.
const (
	KindDokWalt  = "dokwalt"  // dokwalt.db
	KindPostgres = "postgres" // a pg_dump custom-format archive
	KindGlobals  = "globals"  // pg_dumpall --globals-only (roles)
)

type Manifest struct {
	ID         string    `json:"id"`
	Started    time.Time `json:"started"`
	DurationMS int64     `json:"duration_ms"`
	Host       string    `json:"host"`
	Build      string    `json:"build"`
	Trigger    string    `json:"trigger"` // schedule | manual
	Files      []File    `json:"files"`
	Errors     []string  `json:"errors,omitempty"` // what couldn't be backed up
}

type File struct {
	Path     string `json:"path"` // relative to the backup folder
	Kind     string `json:"kind"`
	App      string `json:"app,omitempty"`
	Stage    string `json:"stage,omitempty"`
	Service  string `json:"service,omitempty"`
	Database string `json:"database,omitempty"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

// Size is the total size of the backup's files.
func (m Manifest) Size() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// File finds a file by its path.
func (m Manifest) File(path string) (File, bool) {
	for _, f := range m.Files {
		if f.Path == path {
			return f, true
		}
	}
	return File{}, false
}

// ServiceDir is the folder of one Postgres service's files.
func ServiceDir(app, stage, service string) string {
	return app + "-" + stage + "-" + service
}

// Key is the object key of a file of a backup.
func Key(prefix, id, path string) string {
	return strings.TrimSuffix(prefix, "/") + "/" + id + "/" + path
}

// SplitKey returns the backup ID and the file path of an object key under
// prefix, or ok=false for keys outside a backup folder.
func SplitKey(prefix, key string) (id, path string, ok bool) {
	rest, found := strings.CutPrefix(key, strings.TrimSuffix(prefix, "/")+"/")
	if !found {
		return "", "", false
	}
	id, path, ok = strings.Cut(rest, "/")
	if !ok || path == "" {
		return "", "", false
	}
	if _, err := ParseID(id); err != nil {
		return "", "", false
	}
	return id, path, true
}

func ParseID(id string) (time.Time, error) {
	t, err := time.Parse(IDFormat, id)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid backup id %q", id)
	}
	return t, nil
}

// Keep picks the backups retention keeps: the newest backup of each of the
// last `daily` days that have one, and of each of the last `weekly` ISO
// weeks that have one (the two overlap, like restic's --keep-daily and
// --keep-weekly). Days and weeks are taken in loc. IDs that don't parse are
// kept: they aren't ours to delete.
func Keep(ids []string, daily, weekly int, loc *time.Location) map[string]bool {
	type entry struct {
		id string
		t  time.Time
	}
	var all []entry
	keep := map[string]bool{}
	for _, id := range ids {
		t, err := ParseID(id)
		if err != nil {
			keep[id] = true
			continue
		}
		all = append(all, entry{id, t.In(loc)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.After(all[j].t) })
	days, weeks := map[string]bool{}, map[string]bool{}
	for _, e := range all {
		day := e.t.Format("2006-01-02")
		if !days[day] && len(days) < daily {
			days[day] = true
			keep[e.id] = true
		}
		y, w := e.t.ISOWeek()
		week := fmt.Sprintf("%d-W%02d", y, w)
		if !weeks[week] && len(weeks) < weekly {
			weeks[week] = true
			keep[e.id] = true
		}
	}
	return keep
}

// ParseClock parses a daily time like "03:00".
func ParseClock(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid time %q (want HH:MM, like 03:00)", s)
	}
	return t.Hour(), t.Minute(), nil
}

// Due reports whether the scheduled backup should run now: the daily time
// has passed today and today's run hasn't been attempted (lastDay is the
// date, in loc, of the last scheduled attempt). A server that was down at
// the scheduled time catches up when it comes back, the same day.
func Due(now time.Time, clock, lastDay string) bool {
	h, m, err := ParseClock(clock)
	if err != nil {
		return false
	}
	today := now.Format("2006-01-02")
	at := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	return !now.Before(at) && lastDay != today
}

// Next returns the next scheduled run after now.
func Next(now time.Time, clock, lastDay string) time.Time {
	h, m, err := ParseClock(clock)
	if err != nil {
		return time.Time{}
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if lastDay == now.Format("2006-01-02") {
		return at.AddDate(0, 0, 1)
	}
	if now.After(at) {
		return now // overdue: runs within a minute
	}
	return at
}
