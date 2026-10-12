package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/client"
)

// The rename warning says what the downtime covers, from the app's volumes.
func TestRenameWarning(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	vols := map[string][]api.AppVolume{
		"landing": {},
		"shop":    {{Stage: "production", Name: "cachedata"}, {Stage: "staging", Name: "cachedata"}},
	}
	mux.HandleFunc("GET /v1/apps/{app}/volumes", func(w http.ResponseWriter, r *http.Request) {
		v, ok := vols[r.PathValue("app")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(api.Error{Error: `app "` + r.PathValue("app") + `": not found`})
			return
		}
		json.NewEncoder(w).Encode(v)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	s := &session{name: "prod", c: client.Local(sock)}

	for app, want := range map[string]string{
		"landing": "its site is down for a few seconds (no volumes to copy)",
		"shop":    "while its volumes are copied: cachedata, cachedata (staging).",
	} {
		got, err := renameWarning(t.Context(), s, app)
		if err != nil || !strings.Contains(got, want) {
			t.Errorf("%s: %q, %v; want it to contain %q", app, got, err, want)
		}
	}
	if _, err := renameWarning(t.Context(), s, "nope"); err == nil || !strings.Contains(err.Error(), `app "nope"`) {
		t.Errorf("an unknown app must fail before asking, got %v", err)
	}

	// A server without the volumes endpoint: the generic warning.
	mux2 := http.NewServeMux()
	sock2 := filepath.Join(t.TempDir(), "old.sock")
	ln2, _ := net.Listen("unix", sock2)
	srv2 := &http.Server{Handler: mux2}
	go srv2.Serve(ln2)
	t.Cleanup(func() { srv2.Close() })
	got, err := renameWarning(t.Context(), &session{name: "prod", c: client.Local(sock2)}, "landing")
	if err != nil || !strings.Contains(got, "volumes (if any)") {
		t.Errorf("old server: %q, %v", got, err)
	}
}
