# Command reference

*Every DokWalt command and flag, with a one-line description and an example.*

Run `dokwalt <command> --help` for details. In the examples, the current
folder is linked to the app `blog`, so `-a` is omitted.

## Global flags

| Flag                | Meaning                                              |
|---------------------|------------------------------------------------------|
| `-a, --app <name>`  | app to act on (default: the app linked to this folder) |
| `-s, --stage <st>`  | `staging` or `production` (pipeline apps only; default `production`) |
| `--server <name>`   | server context to use (default: see below)           |
| `--json`            | machine-readable JSON output                         |

## How the app and server are chosen

**App**: `-a` → the folder link (current folder or nearest parent) →
otherwise the error `which app? pass -a <app>, or run dokwalt link <app>`.

**Server**: `--server` → the server recorded with the folder link → the
current server (`dokwalt server use`) → the only configured server.

**Server targets** (`server init`, `server add`): `user@host`,
`user@host:port`, or any alias from `~/.ssh/config`. The SSH settings are
resolved with `ssh -G`, `ProxyJump` works, and agent keys are tried before
identity files.

## Environment variables

| Variable             | Effect                                                   |
|----------------------|----------------------------------------------------------|
| `DOKWALT_CONFIG`     | local config file (default `~/.config/dokwalt/config.json`: servers, current server, folder links) |
| `DOKWALT_SSH_CONFIG` | SSH config file passed to `ssh -G` instead of `~/.ssh/config` |

## Output modes

- On a terminal: colors, symbols (✓ ! ✗ ◆ →), live progress, and a
  typed-name confirmation for `apps:destroy` and `pipeline:disable`.
  `rollback` and `promote` don't ask.
- Not a TTY (pipes, scripts, cron): plain text, one line per step.
- `--json`: read commands print JSON. `logs --json` prints NDJSON, one
  object per line.

## Servers

```bash
# Install Docker, the daemon (systemd) and Caddy over SSH; asks for sudo
dokwalt server init dd@pi.home --email you@example.com
# Register an already-initialized server under a name
dokwalt server add vps deploy@203.0.113.10
# List server contexts
dokwalt server list
# Switch the current server
dokwalt server use pi
# Forget a server context (nothing is changed on the server)
dokwalt server remove vps
# Versions, resources, daemon and Caddy memory (RSS and heap)
dokwalt server info
dokwalt server settings [key=value...]   # acme_email, keep_releases, drain, metrics_retention_days
# Re-run the idempotent install with this CLI's version; apps keep serving
dokwalt server upgrade
```

`server init` flags: `--name` (context name, default: the host's first
label, or `default` for an IP), `--email` (Let's Encrypt), `--binary`
(local linux binary). Without `--binary`, the CLI uses
`dokwalt_linux_<arch>` next to itself or in `dist/`, or downloads it from
GitHub releases and checks it against `checksums.txt`. The new server
becomes current if none is set. `server upgrade --binary <path>` works the
same way.

## Apps

```bash
# List apps with stage, status, release, domains
dokwalt apps
# Create an app on the current server and link this folder to it
dokwalt apps:create blog
dokwalt apps:create blog --no-link        # don't link the folder
# Stages, services, domains, releases of an app
dokwalt apps:info
# Delete an app with its containers, volumes and data (type the name)
dokwalt apps:destroy blog
dokwalt apps:destroy blog --confirm blog  # no prompt, for scripts
# Export config (clear text!), domains, overrides; stdout without a file
dokwalt apps:export blog.json
# Recreate an app from an export on the current server (-a renames it)
dokwalt apps:import blog.json
# Link this folder to an app (stored in the local config, not the repo)
dokwalt link blog
# Remove this folder's link
dokwalt unlink
```

`apps:export <file>` writes mode 0600. Images and volume data aren't included.

## Deploy & releases

```bash
# Build on the Mac, upload changed images, roll out with zero downtime
dokwalt deploy -m "fix feed"
dokwalt deploy -f compose.yaml -f compose.prod.yaml
# List releases, newest first
dokwalt releases -n 30
# Roll back to the previous successful release, or to vN (no prompt)
dokwalt rollback v12
```

- `deploy`: `-m, --message` (default: last commit subject), `--no-cache`,
  `-f, --file` (repeatable; default `compose.yaml` / `docker-compose.yml`).
- `releases`: `-n, --limit` (default 15).
- `rollback [vN]`: `--with-config` also restores that release's config.

## Config

```bash
# List config vars, masked (first 2 characters + •••)
dokwalt config
dokwalt config --reveal                   # clear text
dokwalt config --shell > .env.backup      # KEY="value" lines, clear text
# Print one value
dokwalt config:get DATABASE_URL
# Set vars: new config version + new release (zero downtime)
dokwalt config:set SMTP_HOST=smtp.example.com SMTP_PORT=587
dokwalt config:set FEATURE_X=on --no-restart   # applies on next deploy
# Remove vars (new release)
dokwalt config:unset DEBUG
# Import a .env file (comments, export, quotes): one release
dokwalt config:import .env.production
```

## Domains & health

```bash
# List domains, targets and reachability
dokwalt domains
dokwalt domains --no-check                # skip the probes
# Route a domain to a service (always added; prints reachability)
dokwalt domains:add blog.example.com --service web --port 3000
# Remove a domain or a redirect
dokwalt domains:remove blog.example.org
# Redirect one domain to another (308, path and query kept)
dokwalt domains:redirect www.blog.example.com blog.example.com
# Stricter HTTP check during deploys: GET path must be 2xx/3xx
dokwalt healthcheck:set --service web --path /healthz --timeout 90s
# Back to the default check (any answer below 500 on /)
dokwalt healthcheck:unset --service web
```

`domains:add`: `--service` defaults to the only stateless service that
declares a port. `--port` defaults to its first declared port, or 80 with a
warning. `healthcheck:set --timeout` defaults to `60s`.

## Services & runtime

```bash
# Deploy mode (stateful or blue/green), why, health check per service
dokwalt services
# Override stateful detection; redeploys the current release right away
dokwalt services:set cache --stateful=false
# Containers with state, health, restarts
dokwalt ps
# Restart active containers one at a time (no new release, jobs skipped)
dokwalt restart --service worker
# Stop the stage; domains show a maintenance page; survives reboots
dokwalt stop
# Start a stopped stage again
dokwalt start
# Merged logs
dokwalt logs -f --service web --since 10m --grep error -n 200
# Shell or command in a running container, over SSH
dokwalt exec --service web -- sh
```

- `logs`: `-f, --follow`, `--service`, `--grep <text>` (case-insensitive
  substring), `-n, --lines` (default 100), `--since <duration>` (`30m`,
  `2h`).
- `exec [--service] [-- <cmd>]`: default service is the first stateless
  service that isn't a one-shot job, and the default command is `sh`.

## Databases

```bash
# Tunnel to the database and open psql / mysql / mongosh / redis-cli
dokwalt db:connect
# Keep a tunnel open on a fixed local port for a GUI tool
dokwalt db:connect --tunnel-only --port 5433
```

Flags: `--service` (default: the first recognised database image, SQL
preferred), `--tunnel-only` (print the URL and wait), `--port` (local port,
default random), `--remote-port` (container port, default: the database's
standard port or the first declared port).

```bash
# Download a Postgres dump (pg_dump -Fc) to this folder
dokwalt db:backup
dokwalt db:backup -o ~/Backups/blog.dump
```

Flags: `-o, --output` (default `<database>-<stage>-<time>.dump`),
`--service`, `--database`. Both commands also work from an app that uses a
shared database (`x-dokwalt.uses`): see `dokwalt docs databases`.

## Backups

Server-wide: no app needed. Details: `dokwalt docs backups`.

```bash
# Nightly backups to R2: every Postgres database, their roles, dokwalt.db
dokwalt backup:setup --r2-account <id> --bucket backups --access-key <key>
dokwalt backup:setup --time 04:30 --keep-daily 14   # change one setting
dokwalt backup:now                                   # one now
dokwalt backups                                      # setup, last run, backups in the bucket
dokwalt backups latest                               # files of one backup
dokwalt backup:download latest [file] [-o folder]
dokwalt backup:restore latest --database blog        # or a file path; --confirm blog
dokwalt backup:disable
```

`backup:setup` flags: `--r2-account` or `--endpoint` + `--region`, `--bucket`,
`--access-key`, `--secret-key-stdin` (else a prompt or
`DOKWALT_BACKUP_SECRET_KEY`), `--prefix` (default `dokwalt/<hostname>`),
`--time` (default `03:00`, server local time), `--keep-daily` (7),
`--keep-weekly` (4).

## Pipelines

```bash
# Add a staging stage next to production
dokwalt pipeline:enable
# Delete staging: containers, volumes, config, domains (type the name)
dokwalt pipeline:disable
dokwalt pipeline:disable --confirm blog
# Release staging's current images to production (no build, no prompt)
dokwalt promote
```

## Monitoring & alerts

```bash
# Live CPU, memory, network per app and service, HTTP per domain
dokwalt top
# History as sparklines (default 24h; e.g. 1h, 24h, 7d)
dokwalt metrics --since 7d
# Channels, thresholds, what is firing
dokwalt alerts
# Add a Discord or Slack webhook
dokwalt alerts:add slack https://hooks.slack.com/services/T0/B0/xyz
# Remove a channel by id
dokwalt alerts:remove 2
# Send a test message to every channel
dokwalt alerts:test
# Thresholds: disk, memory (%), temperature (°C), restarts (per 10 min)
dokwalt alerts:set disk=85 temperature=75 restarts=5
# Diagnose this Mac, the server, Caddy, apps and domains
dokwalt doctor
```

## Help

```bash
# Full-screen dashboard (without a terminal: prints help)
dokwalt
# Built-in docs; without a topic, list topics
dokwalt docs domains
dokwalt docs --search rollback            # topics containing these words
dokwalt docs releases --no-pager          # print without a pager
# CLI version: dokwalt <build> (API <v>, os/arch)
dokwalt version
```

## Common combinations

```bash
dokwalt -a shop --server vps logs -f
dokwalt -s staging releases --json | jq '.[0].status'
```

See also: `dokwalt docs getting-started`, `dokwalt docs concepts`, `dokwalt docs deploy`, `dokwalt docs troubleshooting`
