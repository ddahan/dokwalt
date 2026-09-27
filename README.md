# DokWalt

**Deploy Docker Compose apps to your own server — Heroku's workflow, none of the bloat.**

DokWalt is a single small binary. On your laptop it's a polished CLI (and a live
full-screen dashboard); on your server it's a ~27 MB daemon next to Caddy. Your
`docker-compose.yml` stays exactly as it is.

```text
$ dokwalt deploy
◆ Deploying shop  to prod (linux/arm64)
✓ Built migrate, web 12.4s
✓ Uploaded 16.6 MiB in 0.6s
✓ Services — cache: stateful (named volume cachedata), migrate: one-shot job, web: stateless
✓ Created release v42
✓ Stateful services ready 0.8s
✓ Containers running 4.1s
✓ web is healthy 1.2s
✓ v42 is live

✓ shop is live at https://shop.example.com
```

## Why

- **Zero-downtime deploys.** The new version starts next to the old one, must pass
  its health checks, then traffic switches atomically. If anything fails, the
  current version keeps serving — and you see the app's own error logs.
- **Survives reboots and power cuts.** A reconciler under systemd converges the
  server back to its desired state. Tested: sites are back in seconds after a hard
  power cut, with no manual action.
- **Heroku's good ideas.** Immutable numbered releases, instant rollbacks, config
  as env vars (never in your repo), and optional pipelines where `promote` ships
  staging's exact images to production — no rebuild, no upload.
- **Native Docker Compose.** Databases and caches (anything with a named volume or
  a known database image) are never duplicated and restart only when their own
  definition changes. One-shot jobs (`service_completed_successfully`) act as a
  release phase for migrations.
- **Automatic HTTPS** with Let's Encrypt via Caddy, and a real end-to-end check
  that your DNS and ports are right before you wonder why the certificate fails.
- **Small footprint.** No web UI, no Swarm, no Kubernetes, no Redis, no Postgres
  for itself (SQLite). Built for a 1 GB VPS or a Raspberry Pi 5.
- **Nothing exposed.** The control plane is a Unix socket reached over your SSH
  connection. Only Caddy listens on 80/443; apps and databases stay on private
  networks (and can't reach each other).

## Quick start

```bash
# On your Mac (needs Docker Desktop or OrbStack for builds)
make build && sudo cp bin/dokwalt* /usr/local/bin/

# Install on a fresh Debian/Ubuntu/Raspberry Pi OS (64-bit) server
dokwalt server init deploy@203.0.113.10 --email you@example.com

# In your project folder (with a compose file)
dokwalt apps:create shop
dokwalt config:set SECRET_KEY=... DATABASE_PASSWORD=...
dokwalt deploy
dokwalt domains:add shop.example.com --service web
```

Then just `dokwalt` for the live dashboard.

## Everyday commands

| | |
|---|---|
| `dokwalt deploy` | build here for the server's CPU, upload changed images, roll out |
| `dokwalt releases` / `rollback [vN]` | history and instant rollback |
| `dokwalt config:set K=V` | config vars (encrypted at rest; applied with zero downtime) |
| `dokwalt domains:add` / `domains:redirect` | routing and automatic HTTPS |
| `dokwalt logs -f` | merged, colored, live logs of every service |
| `dokwalt top` / `metrics` | live CPU/RAM/network/traffic, and 7-day history |
| `dokwalt db:connect` | psql/mysql/mongosh/redis-cli through SSH, or a tunnel for your GUI |
| `dokwalt exec -- sh` | a shell in a running container |
| `dokwalt pipeline:enable` / `promote` | staging → production with the same images |
| `dokwalt alerts:add discord <url>` | site down, crash loop, disk, temperature… |
| `dokwalt doctor` | diagnose the server, apps, DNS and this machine |
| `dokwalt docs [topic]` | the full documentation, in your terminal |

## How it works

```text
 Mac                              Server (systemd)
 ─────────────────                ──────────────────────────────────────────────
 dokwalt CLI  ──SSH── dial-stdio ─▶ dokwalt daemon (Unix socket, SQLite)
   │ docker compose build            │  docker compose ─▶ dw-shop-production-data   (db, cache)
   │ docker save | zstd ─────────────▶│                   dw-shop-production-blue   (web, worker)
                                     │                   dw-shop-production-green  (next release)
                                     └─ Caddy (80/443, Let's Encrypt) ─▶ active color
```

- Builds happen on your machine (`docker buildx` for linux/amd64 or arm64);
  images are content-addressed, so unchanged images are never uploaded twice.
- The daemon splits your compose project into a **data** project (stateful
  services, never duplicated) and a **blue/green** project (everything else).
- Config values only ever live in the daemon's encrypted SQLite store and in the
  environment of the `docker compose` process — never in files, never in git.
- Caddy is the only public listener. Its config is written to disk *and* loaded
  atomically, so after a reboot it serves the last-known-good routes even before
  the daemon starts.

Details: [`docs/SPEC.md`](docs/SPEC.md) and `dokwalt docs concepts`.

## Raspberry Pi

`dokwalt docs raspberry-pi` is a complete installation guide: hardware (use an
NVMe SSD, not an SD card), Raspberry Pi OS Lite 64-bit, memory cgroup, watchdog,
home networking (port forwarding, dynamic DNS, CGNAT), hardening, backups and a
final checklist including a real power-cut test.

## Development

```bash
make build   # bin/dokwalt + linux server binaries (used by `server init`)
make test    # unit tests
make e2e     # full end-to-end run against a throwaway systemd+Docker server container
make dist    # release binaries + checksums
```

The e2e suite installs DokWalt on a privileged Debian container and checks
deploys under load (0 failed requests), config releases, rollback, pipelines,
failed deploys, the DB tunnel, reboot and power-cut recovery, and the daemon's
memory budget.

## Status

v0.1 — the core workflow is implemented and tested end to end. Not yet: GitHub
push-to-deploy, review apps, layer-aware image transfer, DNS-01 certificates. See the implementation status in `docs/SPEC.md`.

MIT licensed. No telemetry.
