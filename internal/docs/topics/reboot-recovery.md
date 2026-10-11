# Reboot recovery

*Why sites break after a reboot on other setups, and how DokWalt brings every site back on its own.*

After a `sudo reboot`, a kernel update at 04:00 or a power cut, every site must serve HTTPS again
**without you typing anything**. This page explains the mechanisms and how to test them.

## Why sites break elsewhere

| Common cause | What DokWalt does instead |
|---|---|
| The orchestrator is itself a container, relying on Docker restart policies or Swarm state | The daemon is a native systemd service, ordered after and requiring Docker |
| The proxy starts empty and discovers backends late | Caddy boots from the last config file on disk; the daemon re-applies routes computed from SQLite and running containers |
| Containers made with `docker run` and no restart policy, or removed | Every service gets `restart: unless-stopped`; the reconciler recreates anything missing |
| A deploy was half done at reboot time (two versions, proxy on a dead one) | Startup recovery marks it failed and tears down the unfinished color |
| An old color left running after an interrupted drain | The reconciler removes it (never while it is still draining) |
| Certificates lost or re-requested (and rate limited) | Certificates persist in the Docker volume `dokwalt-caddy-data` |
| SD card or flash corruption | An SSD is recommended (`dokwalt docs server`); SQLite in WAL mode |
| Wrong clock at boot, TLS failures | `doctor` checks clock sync; Caddy retries |
| Restarting Docker kills all containers | Docker `live-restore: true`, set by `server init` |

## The systemd unit

`server init` writes `/etc/systemd/system/dokwalt.service` (see it with `systemctl cat dokwalt`):

```ini
[Unit]
Description=DokWalt — Docker Compose apps with zero-downtime deploys
Documentation=https://github.com/ddahan/dokwalt
After=docker.service network-online.target
Wants=network-online.target
Requires=docker.service

[Service]
Type=notify
ExecStart=/usr/local/bin/dokwalt daemon
Restart=always
RestartSec=2
# systemd restarts the daemon if its main loop hangs.
WatchdogSec=60
TimeoutStartSec=120
TimeoutStopSec=15
Environment=GOMEMLIMIT=40MiB
Environment=HOME=/root
RuntimeDirectory=dokwalt
RuntimeDirectoryMode=0755
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=full
ProtectKernelTunables=yes
ProtectControlGroups=no
LockPersonality=yes

[Install]
WantedBy=multi-user.target
```

- **`After=` / `Requires=docker.service`**: the daemon starts after Docker; if Docker stops, the
  daemon stops too, and `Restart=always` brings it back (2 s later) once Docker is back.
- **`Type=notify`**: `READY=1` is sent as soon as the API socket listens, well within
  `TimeoutStartSec=120`. The status (`systemctl status dokwalt`) then moves through
  `starting: waiting for docker` → `recovering` → `running`.
- **`WatchdogSec=60`**: the daemon pings systemd while its main loop heartbeat is less than
  10 minutes old; a stuck main loop stops the pings and systemd restarts it.
- **`HOME=/root`**: the `docker compose` processes the daemon runs use root's Docker config, so
  private registries work after `sudo docker login` on the server.
- **`GOMEMLIMIT=40MiB`**: keeps the Go heap small; the daemon also disables transparent huge
  pages for itself at start (and re-execs), which keeps it around 27 MiB RSS.

The daemon is **never** a container: the component that repairs containers must not depend on
Docker's restart policies to come back itself.

## The reconciler

Desired state lives in SQLite (apps, stages, current release, active color, stopped flag). Actual
state is read from Docker through the labels on every container: `dokwalt.app`, `dokwalt.stage`,
`dokwalt.service`, `dokwalt.role` (`data` or `app`), and `dokwalt.color` + `dokwalt.release` on
stateless services.

**When it runs:**

- at startup, after waiting for the Docker API (backoff 0.5 s → 10 s) and after startup recovery;
- every 60 s;
- 3 s after a Docker `die`, `oom` or `destroy` event for a DokWalt container (bursts are
  coalesced; the event stream reconnects on its own), and after `dokwalt start`.

**What each pass does:**

1. If an operation (deploy, rollback, config change, stop…) holds the server-wide lock, it skips
   the pass: that operation owns the state right now.
2. Ensures the Caddy project `dokwalt-system` runs; the container is recreated only if it isn't
   running or its compose definition changed.
3. For each stage with a current release: ensures network `dw-<app>-<stage>` exists and attaches
   `dokwalt-caddy` to it. Stopped stages are skipped from here on.
4. For the data project (`dw-<app>-<stage>-data`) and the active color project
   (`dw-<app>-<stage>-blue` or `-green`): if any service other than a one-shot job has no running
   container, it creates the volumes, writes the compose file and runs
   `docker compose up -d --remove-orphans` for that project. On error it alerts
   "Self-healing failed"; the alert resolves when a later pass succeeds.
5. Removes color projects of known stages that are neither active nor still draining.
6. Re-applies the Caddy config if the desired one differs from the file or the loaded config.

It never removes volumes and only touches projects of stages it knows. It is cheap when nothing
is wrong: a few Docker API calls, no compose invocation. A crash-looping container counts as
running: the reconciler doesn't fight it; the crash-loop alert and `doctor` report it. One-shot
jobs (migrations) are never restarted by it.

## Caddy and certificates

- The daemon writes the full Caddy config to `/var/lib/dokwalt/caddy/config/caddy.json` on every
  change (temp file + rename, then loaded through the admin socket). Caddy (`restart: always`)
  starts from that file at boot, **before the daemon is even up**, and routes to container names
  (e.g. `dw-blog-production-green-web-1:3000`) that resolve as soon as the containers run.
- Certificates and the ACME account live in the Docker volume `dokwalt-caddy-data` (Caddy's
  `/data`), so a reboot needs no contact with Let's Encrypt. Renewals continue as usual.

## Docker live-restore

`live-restore: true` in `/etc/docker/daemon.json` keeps containers running while `dockerd`
itself restarts (package upgrades, crashes). It does not apply across a machine reboot: there,
restart policies start the containers and the reconciler checks and repairs the result.

## Half-finished deploys

At startup, once Docker answers and before the first reconcile pass, every release still
`deploying` is marked `failed` ("interrupted (daemon restart or reboot during deploy)") and its
color project is brought down if it isn't the active color. The stage keeps its previous current
release, which keeps serving; deploy again when ready. An old color whose drain was interrupted is
removed by the reconciler.

## Stopped apps stay stopped

`dokwalt stop` records the stage as stopped and runs `docker compose stop`; with
`restart: unless-stopped` those containers don't start at boot, and the reconciler skips the
stage. Its domains answer 503 `<app> is temporarily unavailable for maintenance.` and site-down
alerts skip it. `dokwalt start` brings it back.

## Boot sequence

| Step | Event |
|---|---|
| 1 | systemd starts `docker.service`; containers with `unless-stopped`/`always` start, **Caddy included, with its last config** |
| 2 | `dokwalt.service` starts; the API socket listens (`READY=1`) |
| 3 | The daemon waits for the Docker API, recovers interrupted deploys, reconciles |
| 4 | Sites answer over HTTPS with the persisted certificates as soon as their containers are up |

Apps must retry their database connection at startup: after a reboot all containers start at
once, so a web container may briefly come up before Postgres accepts connections.

## The hard-reboot test

Run it once after installation and after any big change. From your computer:

```bash
ssh -t prod sudo reboot; date
until curl -sfo /dev/null https://blog.example.com; do sleep 2; done; date
dokwalt doctor
```

**Pass:** every site returns 200 over valid HTTPS within 2 minutes, with no command sent to the
server; `doctor` exits 0; `dokwalt releases` is unchanged.

## The power-cut test

1. Load a site in a browser and pull the power plug (on a VPS: hard reset from the provider panel).
2. Wait 10 seconds, plug back in, run the same `until` loop.
3. Check `dokwalt doctor`, `dokwalt releases` (a deploy running at the cut must show `failed` with
   the previous release live), and on the server:

```bash
sudo sqlite3 /var/lib/dokwalt/dokwalt.db 'PRAGMA integrity_check'    # ok
```

The project's own `make e2e` runs both scenarios on every change: the throwaway server is a
privileged Debian container running systemd, Docker and sshd; a reboot is `docker restart` of that
container, a power cut is `docker kill` then `docker start`. Each time the site must answer again
with the latest release, with no command sent, and the script prints how long it took.

## After a reboot: what to check

```text
$ dokwalt doctor
◆ Doctor

✓ Local Docker             Docker 28.4.0 (builds run here)
✓ Connection               prod (deploy@203.0.113.10), daemon 0.1.0 on linux/arm64
✓ Docker                   Docker 28.4.0 (API 1.51), 4 CPUs, 7.9 GiB RAM
✓ Docker live-restore      containers keep running while dockerd restarts or upgrades
✓ Memory cgroup            cgroup 2 (systemd driver)
✓ Compose plugin           docker compose 2.39.1
✓ Start on boot: docker    docker starts at boot
✓ Start on boot: dokwalt   dokwalt starts at boot
✓ Disk                     18% used (42.1 GiB of 234.2 GiB)
✓ Memory                   24% used (1.9 GiB of 7.9 GiB)
✓ Temperature              48°C
✓ Clock                    synchronized with NTP
✓ DokWalt daemon           build 0.1.0, 27.3 MiB RSS
✓ Proxy (Caddy)            running, 46.1 MiB RSS, config loaded
✓ App blog                 running, v12
✓ Domain blog.example.com  ok

  16 ok
```

A warning (`!`) or failure (`✗`) comes with a `→` hint. A failed last deploy adds an
`App <name> last deploy` warning; a stopped app shows `stopped`. If something is not green:

```bash
systemctl status docker dokwalt           # both active (running)?
journalctl -u dokwalt -b --no-pager       # this boot's daemon log
journalctl -b -1 -p warning --no-pager    # previous boot's warnings (needs persistent journal)
docker ps -a --filter label=dokwalt.app   # what actually runs
```

See also: `dokwalt docs troubleshooting`, `dokwalt docs server`, `dokwalt docs deploy`,
`dokwalt docs monitoring`.
