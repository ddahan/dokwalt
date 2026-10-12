package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddahan/dokwalt/internal/api"
)

// dbShell's command runs in the database container: here, a real sh with fake
// clients on PATH that print what they received.
func TestDBShell(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  api.DBTarget
		clients []string // fake binaries in the container
		want    string   // client, then arguments
		env     string   // must be in the client's environment
		noArg   string   // must not be in docker exec's arguments
	}{
		{name: "postgres", target: api.DBTarget{Service: "db", Kind: "postgres", Port: 5432, User: "blog", Password: "s3cret", Database: "blog"},
			clients: []string{"psql"}, want: "psql -U blog -d blog -p 5432", env: "PGPASSWORD=s3cret", noArg: "s3cret"},
		{name: "mariadb only", target: api.DBTarget{Service: "db", Kind: "mysql", Port: 3306, User: "root", Password: "pw", Database: "shop"},
			clients: []string{"mariadb"}, want: "mariadb -u root shop", env: "MYSQL_PWD=pw", noArg: "pw"},
		{name: "mongosh", target: api.DBTarget{Service: "mongo", Kind: "mongo", Port: 27017, User: "admin", Password: "p@ss", Database: "app"},
			clients: []string{"mongosh", "mongo"}, want: "mongosh mongodb://admin:p%40ss@127.0.0.1:27017/app?authSource=admin", noArg: "p%40ss"},
		{name: "old mongo, no auth", target: api.DBTarget{Service: "mongo", Kind: "mongo", Port: 27017},
			clients: []string{"mongo"}, want: "mongo mongodb://127.0.0.1:27017/"},
		{name: "valkey", target: api.DBTarget{Service: "cache", Kind: "redis", Port: 6379, Password: "rpw"},
			clients: []string{"valkey-cli"}, want: "valkey-cli -p 6379", env: "REDISCLI_AUTH=rpw", noArg: "rpw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, argv, err := dbShell(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if tc.noArg != "" && strings.Contains(strings.Join(argv, " "), tc.noArg) {
				t.Errorf("password in docker exec's arguments: %q", argv)
			}
			bin := t.TempDir()
			for _, c := range tc.clients {
				script := "#!/bin/sh\necho \"$(basename \"$0\") $*\"\nenv\n"
				if err := os.WriteFile(filepath.Join(bin, c), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin"}, env...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			lines := strings.Split(string(out), "\n")
			if lines[0] != tc.want {
				t.Errorf("ran %q, want %q", lines[0], tc.want)
			}
			if tc.env != "" && !strings.Contains(string(out), "\n"+tc.env+"\n") {
				t.Errorf("%s missing from the client's environment", tc.env)
			}
		})
	}

	t.Run("no client in the container", func(t *testing.T) {
		_, argv, _ := dbShell(api.DBTarget{Service: "db", Kind: "postgres", User: "postgres"})
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = []string{"PATH=" + t.TempDir()}
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "no psql in the db container") {
			t.Errorf("want a clear error, got %v: %s", err, out)
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		_, _, err := dbShell(api.DBTarget{Service: "search", Kind: "unknown"})
		if err == nil || !strings.Contains(err.Error(), "dokwalt exec --service search") {
			t.Errorf("want a hint to use exec, got %v", err)
		}
	})
}
