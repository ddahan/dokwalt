# Troubleshooting

*Symptom → cause → fix for the problems you are most likely to hit, with the exact commands.*

Start with `dokwalt doctor`: most failures show up there with a `→` hint. Commands marked
`# on the server` run over `ssh`; everything else runs on your Mac.

## Connecting

### `HOST KEY MISMATCH`, `host key … not trusted`, or authentication fails

DokWalt uses your SSH settings, so first make plain `ssh` work: `ssh -v dd@pi.home true`.

- **Host key changed** (reinstalled server): always refused; `ssh-keygen -R pi.home`, reconnect.
- **Unknown host:** DokWalt shows the fingerprint and asks. `StrictHostKeyChecking accept-new` (or
  `no`) accepts it without asking; `StrictHostKeyChecking yes` refuses it: add it with `ssh` first.
- **No key offered:** `ssh-add -l` lists agent keys; `ssh-add ~/.ssh/id_ed25519` adds one.
- **Wrong host/user/port/jump host:** DokWalt reads `ssh -G`; check what it will use with
  `ssh -G pi.home | grep -E '^(hostname|user|port|identityfile|proxyjump|stricthostkeychecking) '`.
  A non-default config file must be exported as `DOKWALT_SSH_CONFIG=/path/to/config`.

### `permission denied` on `/run/dokwalt/dokwalt.sock`

**Cause:** your user is not (yet) in group `dokwalt`, or the shell predates `server init`.
Group membership only applies to new logins.

```bash
# on the server
id                                   # must list dokwalt and docker
ls -l /run/dokwalt/dokwalt.sock      # srw-rw---- root dokwalt
sudo usermod -aG dokwalt,docker $USER   # if missing; then log out and back in
```

### `dokwalt: command not found` on the server, or daemon unreachable

`server init` did not finish (it prints the daemon's last 30 journal lines if it failed to start),
or the service is down: `ssh pi.home systemctl status dokwalt`. Re-running `server init` is safe.

### Version mismatch between CLI and daemon

`dokwalt version` prints the CLI's build; `dokwalt doctor` shows the daemon's and warns
(`Versions`) when they differ. `dokwalt server upgrade` installs the CLI's own version on the
server; if the CLI is the older one, update the CLI first.

## Building and images

### `exec format error`

**Cause:** a binary for the wrong architecture. DokWalt already builds for the server
(`DOCKER_DEFAULT_PLATFORM=linux/<server arch>`), so it's usually a Dockerfile downloading an
amd64 binary. **Fix:** a multi-arch base image, and per-architecture assets with `TARGETARCH`:

```text
ARG TARGETARCH
RUN curl -fsSL -o /usr/local/bin/tool https://example.com/tool-linux-${TARGETARCH}
```

### Registry image has no arm64 variant (`no matching manifest`)

The server's `docker compose up` fails and the release is `failed`. Check with
`docker manifest inspect image:tag | grep -c arm64`. Pick another tag or image, or build it from
source with `build:` (native arm64 on Apple Silicon). Avoid QEMU on the Pi.

### Private registry: `pull access denied` / `unauthorized`

Registry images are pulled on the server (only if absent). Log in there, as root:
`ssh -t pi.home sudo docker login registry.example.com`. The daemon runs with `HOME=/root`,
so it uses root's Docker credentials.

### `docker save failed (…) — update Docker Desktop to 28+ for --platform support`

Uploads use `docker save --platform linux/<arch>`: update Docker on the Mac to 28 or newer.

### Transfer interrupted, or a deploy waits before starting

Laptop sleep or Wi-Fi drop during the upload: no release is created; deploy again (images already
on the server are skipped). After the upload the daemon finishes the deploy even if you disconnect.
Mutating operations (deploy, rollback, promote, config changes, stop/start) share one server-wide
lock: a second one waits for the first instead of failing.

## Deploys

### Deploy failed: health check, readiness or a job

The old version keeps serving; the new color is removed and the output shows the last 30 log
lines of the failing container.

```bash
dokwalt releases                        # the failed version and its error
dokwalt logs --service web -n 200 --since 15m
dokwalt config                          # missing variable? typo in DATABASE_URL?
```

| Error | Meaning and fix |
|---|---|
| `health check failed for web: … is the app listening on port 3000?` | Public services get `GET /` on the domain's port: **any status < 500 passes**. The app listens on `127.0.0.1` instead of `0.0.0.0`, or the port is wrong (`domains:remove` + `domains:add --port`) |
| same, after `healthcheck:set --path /healthz` | With a custom path the answer must be **2xx or 3xx**; 4xx now fails |
| timeout (default 60 s) | Slow start: `dokwalt healthcheck:set --service web --path /healthz --timeout 120s` |
| `service web is unhealthy` / `exited with code N` | Its compose `healthcheck` failed, or it crashed; `(out of memory)` = OOM kill. It must also stay up 3 s without restarting |
| `job migrate failed with exit code 1` | A one-shot job (a dependency with `condition: service_completed_successfully`) failed; it runs on every deploy, rollback and promote. Fix the migration; keep migrations backward compatible |
| `timed out after 3m0s waiting for containers to become ready` | A service stayed `starting`; check its healthcheck and logs |

Services without a domain aren't probed over HTTP: running (and healthy, if they have a compose
healthcheck) plus the 3-second stability check is enough.

### Compose warnings and errors

| Message | Meaning and fix |
|---|---|
| ``web: `ports` ignored — only the proxy is public`` | Expected. `dokwalt domains:add <host> --service web --port 3000`; databases via `dokwalt db:connect` |
| `web: env_file ignored — config lives on the server` | Load it once: `dokwalt config:import .env`. Stateful services only get config through `${VAR}` |
| `container_name removed` | It would collide between blue and green; remove it |
| `bind mount of a project file (x) is not possible on the server` | Bake the files into the image (`COPY`), or use a named volume (`uploads:/app/uploads`) |
| `host path /srv/x must exist on the server` | Absolute bind mounts are allowed; create the path on the server |
| `network_mode "host" is not supported` | Use the stage network; services reach each other by name |
| `compose secrets are not supported; use dokwalt config:set` | Move them to config |
| `${X} is used in the compose file but not set` | `dokwalt config:set X=…` |
| (no message) custom `networks:` | Silently replaced by the stage network, service name as alias |

### Rollback fails: images pruned

`image dokwalt/blog-web:… for service web is not on the server (was it pruned? …)`. After each
successful deploy, images beyond the current release and the stage's last 5 earlier releases
are pruned (registry images are never pruned). Check out that commit and run `dokwalt deploy` instead.

## Domains and certificates

### Domain check fails, or no certificate is issued

```bash
dig +short blog.example.com A          # must be your public IP
dig +short blog.example.com AAAA       # empty, or an IPv6 that really reaches the server
curl -4 -s https://ifconfig.me         # your public IPv4 (on the server's network)
curl -I http://blog.example.com        # from a phone on mobile data, not home Wi-Fi
```

- `no DNS record yet`: create the A (and/or AAAA) record, then check again with `dokwalt domains`.
- `DNS points to … but this server's proxy didn't answer on port 80`: wrong IP, or ports
  80/443 not forwarded (home) or blocked (VPS firewall). Port 80 is required for HTTP-01.
- A broken AAAA record: Let's Encrypt prefers IPv6. Fix it or delete it.
- Cloudflare proxy (orange cloud): switch the record to DNS-only until the certificate exists.
- A CAA record that doesn't allow `letsencrypt.org`.
- Hairpin NAT: the check from the server's own LAN fails while the site works from outside.
- `.localhost`, `.test`, `.internal`, `.local`, `.lan`, `.home.arpa` names and IPs get Caddy's
  internal CA certificate (untrusted by browsers, by design).

A "Certificate error" alert fires when Caddy can't get one. Caddy's own log, on the server:
`docker logs --tail 100 dokwalt-caddy 2>&1 | grep -i acme`.

### Rate limited

Nothing to do: Caddy retries by itself with backoff (using Let's Encrypt staging for retries).
Fix the underlying DNS or port problem so the next attempt succeeds.

### TLS errors, "certificate not yet valid", ACME failures: clock wrong

```bash
# on the server
timedatectl                          # System clock synchronized: yes
sudo timedatectl set-ntp true && sudo systemctl restart systemd-timesyncd
```

Outbound UDP 123 must be allowed. On a Pi, the RTC battery keeps time across power cuts.

## Runtime

### Site down after a reboot

```bash
dokwalt doctor
ssh pi.home 'systemctl status docker dokwalt; journalctl -u dokwalt -b --no-pager | tail -50'
ssh pi.home 'docker ps -a --filter label=dokwalt.app'
```

- 503 `<app> is temporarily unavailable for maintenance.`: stopped on purpose, which survives
  reboots: `dokwalt start`. 503 `<app> is not running yet.`: never deployed.
- "Self-healing failed" alert: the reconciler can't restart the stage. After a restore onto a
  new server, built images are missing: `dokwalt deploy` once from each app folder.
- Also check: disk full, clock wrong, or an SD card gone read-only
  (`dmesg | grep -i -e mmc -e 'read-only'`). See `dokwalt docs reboot-recovery`.

### Container restart loop

`dokwalt ps` shows the restart count. A "Crash loop" alert fires at 3 deaths in 10 minutes (tune:
`dokwalt alerts:set restarts=5`; one-shot jobs excluded) and resolves after 10 quiet minutes.

```bash
dokwalt logs --service worker -n 100
ssh pi.home "docker inspect --format '{{.State.OOMKilled}} {{.State.ExitCode}}' <container>"
```

Usual causes: a missing config variable, the database not ready yet after a reboot (make the app
retry its connection), or an out-of-memory kill (see below). Fix and `dokwalt deploy`, or roll
back with `dokwalt rollback`.

### Disk full

```bash
# on the server
df -h / && docker system df
docker image prune                     # dangling images only: safe
sudo journalctl --vacuum-size=100M && sudo apt clean
```

Don't run `docker image prune -a` or `docker volume prune`: the first deletes the images kept for
rollbacks, the second can delete app data. Container logs are capped by the `local` driver
(10 MB × 3 per container). The "Disk almost full" alert fires at 90 % (`alerts:set disk=85`).

### Memory pressure or OOM kills

```bash
dokwalt top                            # which service uses what
ssh pi.home 'free -h; journalctl -k -b | grep -i -e oom -e "killed process"'
```

Set limits in compose (`mem_limit: 256m`), reduce workers (e.g. Gunicorn/Puma processes), add zram
(Pi) or a swap file (VPS), and remember that during a deploy the old and new stateless services
run side by side for a few seconds. An "Out of memory" alert fires on OOM kills.

### `db:connect`: client not found

The local client is missing. Install it, or use `--tunnel-only` with a GUI tool:

```bash
brew install libpq && brew link --force libpq    # psql
brew install mysql-client mongosh redis           # mysql, mongosh, redis-cli
dokwalt db:connect --tunnel-only --port 5433
```

### Pi: throttling or high temperature

`vcgencmd get_throttled`: `0x50000` = under-voltage since boot (official 27 W PSU and cable),
`0x80008` = heat (Active Cooler, fan, airflow). The throttling alert is always on; tune the
temperature one with `dokwalt alerts:set temperature=75`.

## Collecting information for a bug report

```bash
dokwalt version
dokwalt doctor --json > doctor.json
dokwalt server info --json > server-info.json
ssh pi.home 'journalctl -u dokwalt -b --no-pager -n 500' > dokwalt.log
ssh pi.home 'docker logs --tail 200 dokwalt-caddy' > caddy.log 2>&1
```

Include the command you ran and its full output. Review the files first: they contain domain
names. Config values are masked; never attach `config --reveal` output or `apps:export` files.

See also: `dokwalt docs reboot-recovery`, `dokwalt docs domains`, `dokwalt docs deploy`,
`dokwalt docs raspberry-pi`, `dokwalt docs commands`.
