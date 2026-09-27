package alerts

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ddahan/dokwalt/internal/secrets"
	"github.com/ddahan/dokwalt/internal/store"
)

func TestFireRateLimitsAndResolves(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		mu.Lock()
		got = append(got, body["content"])
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"), secrets.NewForTest())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.client = srv.Client()
	st.AddAlertChannel("discord", srv.URL)

	m.Fire("site:x", "Site down", "x is down", true)
	m.Fire("site:x", "Site down", "x is down", true) // rate-limited
	m.Fire("site:x", "Site down", "x is back", false)
	m.Fire("site:x", "Site down", "x is back", false)     // nothing to resolve
	m.Fire("deploy:y", "Deploy succeeded", "y ok", false) // never fired: silent

	if len(got) != 2 || !strings.Contains(got[0], "Site down") || !strings.Contains(got[1], "Resolved") {
		t.Fatalf("messages = %q", got)
	}
	if errs := m.Test(); len(errs) != 0 {
		t.Fatalf("test alert: %v", errs)
	}
	if err := m.SetThreshold("disk", "abc"); err == nil {
		t.Fatal("non-numeric threshold accepted")
	}
}
