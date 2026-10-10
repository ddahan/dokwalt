package backup

import (
	"sort"
	"strings"
	"testing"
	"time"
)

func ids(times ...string) []string {
	var out []string
	for _, s := range times {
		t, _ := time.Parse("2006-01-02 15:04", s)
		out = append(out, t.Format(IDFormat))
	}
	return out
}

func kept(m map[string]bool) string {
	var out []string
	for id := range m {
		t, err := ParseID(id)
		if err != nil {
			out = append(out, id)
			continue
		}
		out = append(out, t.Format("01-02 15:04"))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestKeepDailyAndWeekly(t *testing.T) {
	// Nightly backups from Thu Sep 3 to Sat Oct 10 2026, plus a manual one on Oct 10.
	var all []string
	for d := time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC); !d.After(time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC)); d = d.AddDate(0, 0, 1) {
		all = append(all, d.Format(IDFormat))
	}
	all = append(all, ids("2026-10-10 14:30")...)
	got := kept(Keep(all, 7, 4, time.UTC))
	// 7 days: Oct 4–10 (Oct 10's newest is the manual one). 4 weeks: the newest
	// of weeks 41 (Oct 10 manual), 40 (Oct 4), 39 (Sep 27), 38 (Sep 20).
	want := "09-20 03:00,09-27 03:00,10-04 03:00,10-05 03:00,10-06 03:00,10-07 03:00,10-08 03:00,10-09 03:00,10-10 14:30"
	if got != want {
		t.Fatalf("kept\n got %s\nwant %s", got, want)
	}
}

func TestKeepWithGaps(t *testing.T) {
	// The server was off for a while: the last 7 days *with a backup* are kept.
	all := ids("2026-08-01 03:00", "2026-09-01 03:00", "2026-10-01 03:00", "2026-10-10 03:00")
	if got := kept(Keep(all, 2, 0, time.UTC)); got != "10-01 03:00,10-10 03:00" {
		t.Fatalf("got %s", got)
	}
}

func TestKeepIgnoresForeignNames(t *testing.T) {
	got := Keep([]string{"not-a-backup"}, 1, 1, time.UTC)
	if !got["not-a-backup"] {
		t.Fatal("foreign folder would be deleted")
	}
}

func TestSplitKey(t *testing.T) {
	id, path, ok := SplitKey("dokwalt/pi", "dokwalt/pi/20261010T030000Z/postgres-production-db/blog.dump")
	if !ok || id != "20261010T030000Z" || path != "postgres-production-db/blog.dump" {
		t.Fatalf("got %q %q %v", id, path, ok)
	}
	for _, k := range []string{"dokwalt/pi/.dokwalt-test", "dokwalt/pi/notanid/x", "other/20261010T030000Z/x", "dokwalt/pi/20261010T030000Z/"} {
		if _, _, ok := SplitKey("dokwalt/pi/", k); ok {
			t.Fatalf("%s should not be part of a backup", k)
		}
	}
}

func TestDueAndNext(t *testing.T) {
	at := func(s string) time.Time { t, _ := time.Parse("2006-01-02 15:04", s); return t }
	cases := []struct {
		now, last string
		due       bool
		next      string
	}{
		{"2026-10-10 02:59", "2026-10-09", false, "2026-10-10 03:00"},
		{"2026-10-10 03:00", "2026-10-09", true, "2026-10-10 03:00"},
		{"2026-10-10 15:00", "2026-10-09", true, "2026-10-10 15:00"}, // was down at 03:00: catch up
		{"2026-10-10 15:00", "2026-10-10", false, "2026-10-11 03:00"},
		{"2026-10-10 01:00", "", false, "2026-10-10 03:00"},
	}
	for _, c := range cases {
		if got := Due(at(c.now), "03:00", c.last); got != c.due {
			t.Errorf("Due(%s, last %s) = %v", c.now, c.last, got)
		}
		if got := Next(at(c.now), "03:00", c.last); !got.Equal(at(c.next)) {
			t.Errorf("Next(%s, last %s) = %s, want %s", c.now, c.last, got, c.next)
		}
	}
	if _, _, err := ParseClock("3am"); err == nil {
		t.Error("3am accepted")
	}
}
