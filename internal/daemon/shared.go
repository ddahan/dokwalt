package daemon

import (
	"context"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/ddahan/dokwalt/internal/api"
	"github.com/ddahan/dokwalt/internal/compose"
	"github.com/ddahan/dokwalt/internal/store"
)

// sharedDBTarget resolves db:connect and db:backup for an app whose database
// lives in a shared app (x-dokwalt.uses): the provider's database service,
// reached with the consumer's own role and database when its connection URL
// can be found. Otherwise the provider's superuser is used, on a database
// named after the app.
func (d *Daemon) sharedDBTarget(ctx context.Context, st store.Stage, plan *compose.Plan, port int) (api.DBTarget, error) {
	for _, p := range plan.Uses {
		pst, err := d.engine.ProviderStage(p, st.Name)
		if err != nil {
			return api.DBTarget{}, err
		}
		pplan, err := d.currentPlan(pst)
		if err != nil {
			return api.DBTarget{}, err
		}
		if pplan == nil || len(dbCandidates(pplan)) == 0 {
			continue
		}
		t, err := d.dbTargetOf(ctx, pst, pplan, "", port)
		if err != nil {
			return t, err
		}
		t.Provider = pst.Key()
		cfg, _ := d.store.Config(st.ID)
		if user, pw, db, ok := consumerDBURL(t.Kind, []string{p, p + "-" + t.Service}, plan, cfg); ok {
			t.User, t.Password, t.Database = user, pw, db
		} else {
			t.Database = st.App
		}
		return t, nil
	}
	return api.DBTarget{}, badRequest("no database service detected in %s — pass --service", strings.Join(plan.Uses, ", "))
}

var urlSchemes = map[string][]string{"postgres": {"postgres", "postgresql"}, "mysql": {"mysql"}}

// consumerDBURL finds the connection URL an app uses to reach a shared
// database (host = one of the provider's aliases): config vars first, with
// DATABASE_URL preferred, then the compose environment of its services,
// interpolated with the config.
func consumerDBURL(kind string, hosts []string, plan *compose.Plan, cfg map[string]string) (user, password, database string, ok bool) {
	schemes := urlSchemes[kind]
	if len(schemes) == 0 {
		return "", "", "", false
	}
	var values []string
	keys := store.SortedKeys(cfg)
	sort.SliceStable(keys, func(i, j int) bool { return keys[i] == "DATABASE_URL" && keys[j] != "DATABASE_URL" })
	for _, k := range keys {
		values = append(values, cfg[k])
	}
	if plan.App != nil {
		services, _ := plan.App["services"].(map[string]any)
		names := make([]string, 0, len(services))
		for n := range services {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			svc, _ := services[n].(map[string]any)
			env, _ := svc["environment"].(map[string]any)
			envKeys := make([]string, 0, len(env))
			for k := range env {
				envKeys = append(envKeys, k)
			}
			sort.Strings(envKeys)
			for _, k := range envKeys {
				if s, isStr := env[k].(string); isStr {
					values = append(values, expandVars(s, cfg))
				}
			}
		}
	}
	for _, v := range values {
		u, err := url.Parse(strings.TrimSpace(v))
		if err != nil || !slices.Contains(schemes, u.Scheme) || !slices.Contains(hosts, u.Hostname()) || u.User == nil {
			continue
		}
		pw, _ := u.User.Password()
		return u.User.Username(), pw, strings.TrimPrefix(u.Path, "/"), true
	}
	return "", "", "", false
}

// expandVars interpolates ${VAR}, ${VAR:-default} and ${VAR:?msg} with
// config values, like compose does on the server.
func expandVars(s string, cfg map[string]string) string {
	return os.Expand(s, func(expr string) string {
		name, rest := expr, ""
		if i := strings.IndexAny(expr, ":-?+"); i >= 0 {
			name, rest = expr[:i], expr[i:]
		}
		if v, ok := cfg[name]; ok && v != "" {
			return v
		}
		if def, ok := strings.CutPrefix(rest, ":-"); ok {
			return def
		}
		if def, ok := strings.CutPrefix(rest, "-"); ok {
			return def
		}
		return ""
	})
}
