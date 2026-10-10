# DokWalt — Technical Specification

| | |
|---|---|
| Status | Implemented (v0.1) |
| Audience | Maintainers and contributors |
| Module | `github.com/ddahan/dokwalt` |
| Companion docs | `internal/docs/topics/*.md` (end-user docs, embedded in the binary, rendered by `dokwalt docs`) |

This document describes DokWalt as implemented. The product brief is `docs/PROMPT.md`. When this spec and the code disagree, the code is the reference and this document gets fixed. Terminal output in this document is illustrative: layouts and wording may differ slightly from the real output.

---

## Implementation status

v0.1 implements milestones M1–M4 (§18): server init/upgrade, single-environment and pipeline apps, zero-downtime blue/green deploys, config, releases and rollback, domains with automatic HTTPS, logs, `exec`, `db:connect`, `db:backup`, nightly off-site backups to S3/R2 (§7.7.1), metrics, alerts, the reconciler, `doctor`, the TUI dashboard, `server settings` and the embedded docs. The end-to-end suite (`make e2e`, §18.6) checks every one of these against a throwaway systemd + Docker server.

Still open:

| Item | Status |
|---|---|
| Layer-aware image transfer | Designed (§17.4), not implemented; v0.1 streams whole missing images |
| GitHub push-deploy | Designed (§17.2), not implemented |
| Review apps | Designed (§17.3), not implemented |
| Multi-server polish (fan-out listing, dashboard server switcher) | Contexts work; fan-out not implemented |
| DNS-01 challenges (CGNAT without tunnel tricks, wildcard needs) | Not implemented; HTTP-01 only |

---

## Table of contents

1. [Summary](#1-summary)
2. [Goals & non-goals](#2-goals--non-goals)
3. [Architecture overview](#3-architecture-overview)
4. [Technology choices](#4-technology-choices)
5. [Core concepts & data model](#5-core-concepts--data-model)
6. [App definition](#6-app-definition)
7. [CLI command reference](#7-cli-command-reference)
8. [TUI dashboard](#8-tui-dashboard)
9. [Flows, step by step](#9-flows-step-by-step)
10. [Reboot & self-healing](#10-reboot--self-healing)
11. [Certificates & routing](#11-certificates--routing)
12. [Logs](#12-logs)
13. [Metrics & alerts](#13-metrics--alerts)
14. [Security model](#14-security-model)
15. [Resource budget](#15-resource-budget)
16. [Failure modes & edge cases](#16-failure-modes--edge-cases)
17. [Extensibility](#17-extensibility)
18. [Milestones](#18-milestones)
19. [Installation guides](#19-installation-guides)
20. [Open questions](#20-open-questions)

---

## 1. Summary

DokWalt is a single Go binary that turns one small Linux server (a 1 GB VPS or a Raspberry Pi 5) into a Heroku-like host for many websites defined by their existing `docker-compose.yml`. On the developer's Mac, `dokwalt` is a CLI and full-screen terminal dashboard. On the server, `dokwalt daemon` is a systemd service that keeps all state in SQLite, drives Docker Compose, configures a Caddy reverse proxy with automatic Let's Encrypt certificates, collects metrics and sends Discord/Slack alerts. Above all, it keeps reconciling actual state with desired state, so every site comes back by itself after a reboot or power cut. Images are built on the Mac for the server's architecture and streamed over SSH, and the control plane is only reachable through SSH. Apps get:

- immutable numbered releases;
- encrypted config vars;
- zero-downtime blue/green deploys that never duplicate databases;
- one-shot jobs for migrations;
- instant rollback;
- optional staging → production pipelines with promotion that needs no rebuild.

Nothing else: no web UI, no cluster, no registry, no extra datastore.

---

## 2. Goals & non-goals

### 2.1 Goals

| # | Goal | Verified by |
|---|---|---|
| G1 | Sites survive reboots and power cuts with zero manual action | e2e reboot and power-cut steps (§10.6) |
| G2 | Deploy an unmodified `docker-compose.yml` app | e2e sample app; transformation rules (§6.3) |
| G3 | Zero-downtime deploys, config changes and rollbacks for stateless services | e2e: 0 failed requests out of ~4,000 during a deploy |
| G4 | Databases are never duplicated and never exposed publicly | Transformation rules (§6.3), private networks (§14.4) |
| G5 | Small footprint | Daemon ≈ 27 MiB RSS, Caddy ≈ 46 MiB RSS, total < 100 MB (§15) |
| G6 | Everything from the terminal, polished and scriptable | `--json` on read commands, plain output when not a TTY, dashboard |
| G7 | Secure by default | No public control-plane port; only Caddy publishes ports; secrets encrypted at rest |
| G8 | Heroku ergonomics | apps, releases, config vars, rollback, pipelines, promote |
| G9 | Single-environment apps pay nothing for pipelines | Stage never shown or asked for unless `pipeline:enable` was run |

### 2.2 Non-goals (v1)

- Web UI or browser dashboard.
- Docker Swarm, Kubernetes, clusters, multi-node scheduling. (Several *independent* servers via contexts: yes.)
- Managed databases/add-ons. (One app may still *use* another app's database server, §6.3 `x-dokwalt.uses`; the provider stays an ordinary app.) Off-site backups cover Postgres and DokWalt's own state only (§7.7.1); other stateful services are backed up by hand.
- GitHub integration, push-to-deploy, review apps (the architecture leaves room, §17).
- CI/CD, buildpacks, server-side builds, paid external services.
- Multi-user accounts and roles.
- Redis, message queues, a container registry, or any other extra infrastructure.
- Wildcard domains/certificates, DNS-01 challenges.

---

## 3. Architecture overview

### 3.1 Components

| Component | Where | Runs as | Responsibility |
|---|---|---|---|
| `dokwalt` CLI | Mac | user process | Commands, dashboard, builds (`docker compose build`), image export, SSH transport |
| `dokwalt daemon` | Server | systemd service `dokwalt.service`, root | API on a Unix socket, state (SQLite), deploy engine, compose transformation, reconciler, Caddy config, metrics, alerts |
| `dokwalt dial-stdio` | Server | short-lived process per API connection, as the SSH user | Bridges an SSH session's stdin/stdout to the daemon socket (or, with `--tcp`, to a container address for `db:connect`) |
| SQLite `dokwalt.db` | Server | file in `/var/lib/dokwalt` | Desired state, releases, encrypted config, metrics |
| Docker Engine + compose plugin | Server | `docker.service` | Runs app containers and Caddy |
| `dokwalt-caddy` | Server | container `caddy:2-alpine` in compose project `dokwalt-system` | TLS termination, ACME, routing, access logs to the daemon |
| App containers | Server | compose projects `dw-<app>-<stage>-data`, `-blue`, `-green` | The user's services |

### 3.2 Diagram

```mermaid
flowchart LR
  subgraph Mac["Developer Mac"]
    CLI["dokwalt CLI / dashboard"]
    DD["Docker Desktop / OrbStack<br/>(buildx, linux/arm64 or amd64)"]
    CFG["~/.config/dokwalt/config.json"]
    CLI -->|compose config / build / save| DD
    CLI --- CFG
  end

  subgraph Server["Server (VPS or Raspberry Pi)"]
    SSHD["sshd :22"]
    DS["dokwalt dial-stdio<br/>(per connection)"]
    SOCK[["/run/dokwalt/dokwalt.sock<br/>0660 root:dokwalt"]]
    subgraph Daemon["dokwalt daemon (systemd, Type=notify)"]
      API["HTTP/JSON API /v1"]
      DEP["Deploy engine"]
      REC["Reconciler<br/>(startup, events, 60s)"]
      PX["Caddy config generator"]
      MET["Metrics collector (10s)"]
      ALR["Alerter"]
    end
    DB[("SQLite WAL<br/>/var/lib/dokwalt/dokwalt.db")]
    DOCKER["Docker Engine<br/>+ compose plugin"]
    CADDY["dokwalt-caddy<br/>:80 :443 tcp/udp"]
    subgraph NetBlog["network dw-blog-production"]
      DATA["dw-blog-production-data<br/>(db)"]
      COLOR["dw-blog-production-green<br/>(web, worker)"]
    end
    subgraph NetShop["network dw-shop-production"]
      SHOP["dw-shop-production-blue<br/>(web)"]
    end
    CG["/sys/fs/cgroup, /proc,<br/>/sys/class/thermal, vcgencmd"]
  end

  Internet(("Internet")) -->|80/443| CADDY
  CLI ==>|"one SSH connection<br/>N sessions: dokwalt dial-stdio"| SSHD
  SSHD --> DS --> SOCK --> API
  API --> DEP & REC & PX & MET
  DEP & REC --> DOCKER
  Daemon <--> DB
  PX -->|"admin API (unix socket)<br/>POST /load"| CADDY
  CADDY -->|"JSON access + tls logs<br/>(unix socket)"| MET
  CADDY -->|"attached to every<br/>app network"| COLOR & SHOP
  COLOR --> DATA
  MET --> CG
  ALR -->|HTTPS webhooks| Webhooks(("Discord / Slack"))
  DOCKER --> NetBlog & NetShop & CADDY
```

### 3.3 Interaction summary

1. The CLI resolves the server context and the app (§7.1) and opens **one** SSH connection. For each API connection it opens a new SSH *session* running `dokwalt dial-stdio`, and speaks plain HTTP/1.1 over that session's stdin/stdout.
2. The daemon has no authentication of its own. Reaching the socket requires membership in the group `dokwalt`, which in practice means an SSH login as the admin user (§14).
3. The daemon is the only writer of `dokwalt.db`. It uses the Docker Engine API (`/var/run/docker.sock`) for inspection, events, logs, networks, volumes and image load/list/remove. It runs the `docker compose` CLI plugin as a child process for `up`/`down`/`stop`/`start` of rendered projects, so it never reimplements compose semantics (§4.7). Interactive `exec` doesn't go through the daemon: the CLI runs `docker exec -it` in an SSH PTY session (the admin user is in group `docker`).
4. Caddy runs in a compose project the daemon owns (`dokwalt-system`). The daemon generates its full JSON config, writes it to disk (so Caddy boots with the last-known-good config even before the daemon is up) and loads it through the admin API on a Unix socket.
5. Each app stage has its own private bridge network `dw-<app>-<stage>`. The daemon attaches the Caddy container to every app network, so Caddy can reach every public service while apps cannot reach each other.
6. The metrics collector reads cgroup v2 files directly, keeps a live in-memory view, and writes 1-minute aggregates to SQLite. Caddy streams JSON access logs to a Unix socket owned by the daemon, which aggregates them into HTTP metrics.

### 3.4 Server file layout

```text
/usr/local/bin/dokwalt                         binary (same as the Mac's, linux build)
/etc/systemd/system/dokwalt.service            unit (§10.2)
/etc/docker/daemon.json                        merged: live-restore true, log-driver local (if unset)
/run/dokwalt/dokwalt.sock                      API socket, root:dokwalt 0660
/var/lib/dokwalt/
├── dokwalt.db, dokwalt.db-wal, -shm           SQLite (WAL)
├── secret.key                                 32 random bytes, 0600 (created by the daemon)
├── backup-tmp/                                one dump at a time during an off-site backup (§7.7.1), deleted after
├── caddy/
│   ├── config/caddy.json                      last-known-good Caddy config (→ /config in the container)
│   └── run/                                   → /run/caddy: admin.sock, access.sock
├── system/dokwalt-system.json                 compose file of the Caddy project
└── apps/<app>/<stage>/
    ├── dw-<app>-<stage>-data.json             rendered compose file, data project
    ├── dw-<app>-<stage>-blue.json             rendered compose file, blue project
    └── dw-<app>-<stage>-green.json            rendered compose file, green project
Docker volume dokwalt-caddy-data               Caddy /data: certificates, ACME account
Docker volumes dw-<app>-<stage>-<vol>          app data
```

Rendered compose files never contain config values (§6.4).

### 3.5 Local (Mac) state

`~/.config/dokwalt/config.json` (override the path with `DOKWALT_CONFIG`) holds server contexts and folder links, and nothing about app configuration:

```json
{
  "current": "pi",
  "servers": {
    "pi":   { "target": "dd@pi.home" },
    "prod": { "target": "deploy@203.0.113.10:2222" }
  },
  "links": {
    "/Users/dd/Code/blog": { "server": "pi", "app": "blog" }
  }
}
```

A target is `user@host`, `user@host:port` or any `~/.ssh/config` alias. `DOKWALT_SSH_CONFIG` selects the ssh config file passed to `ssh -G`. Downloaded server binaries are cached under the user cache dir (`~/Library/Caches/dokwalt/<version>/` on macOS).

---

## 4. Technology choices

### 4.1 Language: Go, one binary in two roles

**Choice.** Go, `CGO_ENABLED=0`, static binary, cross-compiled for `darwin/arm64`, `linux/amd64`, `linux/arm64` (`make dist` also builds `darwin/amd64`). The same binary is the CLI and, as `dokwalt daemon`, the server.

**Why.** Install and upgrade become a file copy. Go has first-class SSH (`golang.org/x/crypto/ssh`), good Docker integration, and the best TUI ecosystem (Charm).

**One binary, two roles.**
- `server init` and `server upgrade` upload "the same version for the other OS/arch", so CLI/daemon version skew is structurally minimised.
- `dial-stdio` has to exist on the server anyway, and it's the same binary.
- Shared code (API types, compose model) can't drift.
- Cost: the server binary carries CLI/TUI code on disk. Unused code pages are shared and mostly never faulted in; the measured RSS (§15) includes the pages that are.

**Rejected.** Rust (weaker Docker/TUI libraries for this use, slower iteration), Python/Node (runtime dependency, RAM), two separate binaries (version skew, twice the release surface).

### 4.2 Reverse proxy: Caddy 2 as a supervised container

| Criterion | **Caddy 2** | Traefik 3 | nginx + certbot | HAProxy |
|---|---|---|---|---|
| Automatic HTTPS | Built in (retries, staging fallback, internal CA) | Built in | External certbot + cron + reload | External |
| Dynamic config | Admin API, whole-config `POST /load` | Docker labels / file provider | Reload files | Runtime API, limited |
| Coupling to Docker | None (the daemon pushes config) | Label discovery encourages coupling | None | None |
| HTTP/3 | Yes | Yes | Partial | Partial |
| Access logs to a socket | `net` writer | File/stdout | File/syslog | syslog |

**Choice: Caddy.** Traefik's Docker-label discovery is exactly the kind of implicit state that breaks after a reboot: containers start in any order, and the proxy's view is whatever it happened to discover. With Caddy, the daemon computes the whole routing table from SQLite and Docker and pushes it in one piece. The same config is on disk, so Caddy is right at boot even if the daemon isn't running yet. nginx + certbot has more moving parts (cron, reloads, Python).

**Container vs host service: container**, in compose project `dokwalt-system`, `restart: always`.
- Same on Pi OS, Debian and Ubuntu; no distro packaging.
- It has to join the app networks to reach services by container name without published ports. As a host process it would need published ports, which defeat §14.3.
- `restart: always` plus the persisted config means Caddy comes back with Docker, even before the daemon. The daemon doesn't depend on Docker's restart policy for itself (it's a systemd service), which is what the brief requires.

### 4.3 State: SQLite (WAL) via `modernc.org/sqlite`

- Pragmas: `journal_mode=WAL`, `synchronous=NORMAL`, `busy_timeout=10000`, `foreign_keys=ON`, `cache_size=-1024` (1 MiB). Up to 4 connections; WAL lets readers run next to the writer.
- `modernc.org/sqlite` is pure Go, which keeps `CGO_ENABLED=0` and cross-compilation trivial. It's slower than cgo SQLite on heavy workloads, which doesn't matter at a few rows per minute.
- Schema: `CREATE TABLE IF NOT EXISTS` statements embedded in the binary, applied at daemon start. Future incompatible changes will need versioned migrations (§20).
- Rejected: bbolt (no SQL for metrics queries), Postgres (a whole server), JSON files (no atomic multi-row updates).

### 4.4 TUI: Charm stack

`cobra` for commands and help with examples, `lipgloss` for styling, `bubbletea` + `bubbles` for the dashboard and spinners, `glamour` for rendering docs. Output drops colors and spinners when stdout isn't a TTY.

### 4.5 Transport: SSH with `dial-stdio`

- Host, user, port, identity files, `ProxyJump`, `UserKnownHostsFile` and `StrictHostKeyChecking` are resolved by running `ssh -G <target>` (with `-F $DOKWALT_SSH_CONFIG` if set), so `~/.ssh/config` applies exactly as it does for `ssh`. The first `ProxyJump` hop is dialled with `x/crypto/ssh` and the target is reached through it.
- Auth: keys from ssh-agent (`SSH_AUTH_SOCK`) first, then identity files not already offered by the agent.
- Host keys are checked against known_hosts. An unknown host is accepted automatically with `StrictHostKeyChecking=accept-new` (or `no`); otherwise the CLI shows the fingerprint and asks, like `ssh`. With `StrictHostKeyChecking=yes` an unknown host is refused. A changed key is always refused.
- **Why `dial-stdio` rather than `direct-streamlocal` forwarding:** hardened sshd configs often set `AllowTcpForwarding no` / `AllowStreamLocalForwarding no`. A plain exec session works everywhere, and it's also how `docker -H ssh://` works.

**Rejected.** An API port on TCP with mTLS (public attack surface for no benefit); shelling out to `ssh` for every call (harder to multiplex and to drive PTYs).

### 4.6 Image transfer: content-addressed tags, skip what the server has, `docker save | zstd`

| | save + zstd stream (**v0.1**) | Registry over the SSH channel (layer-aware) |
|---|---|---|
| Moving parts | None on the server (daemon loads the tar through the Engine API) | A registry endpoint served by the daemon |
| Transfer on a code-only change | The changed service's whole image | Only changed layers |
| Transfer when nothing changed | Nothing | Nothing |
| Failure modes | Simple, restartable | Registry storage, GC, content-store duality |

How the v0.1 transfer works:
1. **Reproducible IDs.** Builds run with `BUILDX_NO_DEFAULT_ATTESTATIONS=1`. Provenance attestations embed timestamps, so without this flag identical builds get new image IDs.
2. **Content-addressed tags.** Each built image is tagged `dokwalt/<app>-<service>:<first 12 hex of the image ID>`.
3. **Skip.** The CLI calls `POST /v1/images/have` and only sends images the server doesn't have. An unchanged service, or a redeploy of the same code, sends nothing.
4. **Stream.** `docker save --platform linux/<arch> <missing…>` → zstd (`klauspost/compress`, default level) → `POST /v1/images/load` over one SSH session. The daemon decompresses and loads through the Engine API.

Measured: ~16 MiB compressed for a `python:alpine` app, ~0.6 s on a LAN. `docker save --platform` needs Docker 28+ on the Mac.

**Future improvement (§17.4):** layer-aware push to a registry endpoint served by the daemon through `dial-stdio`, backed by Docker's image store, so only missing layers cross the wire.

### 4.7 Compose execution: the `docker compose` plugin on the server

The daemon renders compose JSON files and runs `docker compose --progress plain -p <project> -f <file> up -d --remove-orphans` as a child process. Its environment contains a minimal base environment **plus the stage's decrypted config vars**. Readiness is then checked by the daemon itself (§9.3.1) rather than by compose `--wait`, because it has to treat one-shot jobs, crash loops and HTTP checks together. Reimplementing compose on the Engine API would be a large, bug-prone surface.

### 4.8 Secrets encryption: XChaCha20-Poly1305

`golang.org/x/crypto/chacha20poly1305` (XChaCha20 variant). The key is 32 random bytes in `/var/lib/dokwalt/secret.key` (0600), created by the daemon on first start. Every value is sealed with a fresh random 24-byte nonce (`nonce || ciphertext`), which is safe with random nonces at any volume, unlike AES-GCM's 12 bytes. §14.2 explains what this protects against and what it doesn't.

### 4.9 License: MIT

| | MIT | Apache-2.0 | AGPL-3.0 |
|---|---|---|---|
| Adoption friction | Lowest | Low | High (many companies ban it) |
| Patent grant | Implicit at best | Explicit grant + retaliation | Explicit (v3) |
| Stops a closed hosted fork | No | No | Yes |
| Compatible with deps (Caddy image Apache-2.0, Charm MIT, x/crypto BSD) | Yes | Yes | Yes |

**Choice: MIT.** It's the simplest license with the widest adoption, for a single-admin tool nobody is likely to resell as a service. DokWalt runs Caddy's image rather than linking it, so Apache's patent clause buys little. The accepted trade-off: someone may fork it closed.

---

## 5. Core concepts & data model

### 5.1 Concepts

| Concept | Definition |
|---|---|
| **Server (context)** | A named SSH target stored on the Mac. `current` is used when `--server` is absent. |
| **App** | A name (`^[a-z][a-z0-9-]{0,29}$`, no trailing dash). Its compose model comes with each deploy; its settings live on the server. |
| **Stage** | An environment of an app: always `production`, plus `staging` when the pipeline is enabled. Everything that "belongs to an app" belongs to a stage, so enabling a pipeline needs no migration. |
| **Pipeline** | `apps.pipeline=1`. Unlocks `-s staging` and `promote`, and makes stage names appear in output. |
| **Service** | A compose service, treated in one of three ways. **Stateful**: runs in the data project, never duplicated. **Stateless**: blue/green. **One-shot job**: runs to completion in the new color before its dependents start. A service is **public** if at least one domain targets it. |
| **Release** | Immutable, numbered per stage (`v1`, `v2`, …): images (service → ref + ID), the user's normalized compose model, config version, color, description, git SHA, status. Created by deploy, config change, rollback, promote and stateful overrides. |
| **Config** | Encrypted key/value vars per stage. Every change bumps the stage's config version and (unless `--no-restart`, or never deployed) creates a new release. |
| **Color** | `blue` or `green`: the compose project that hosts the stateless services of a release. Alternates on every release. |
| **Data project** | `dw-<app>-<stage>-data`, holding the stateful services. Updated in place; compose recreates a container only when its definition changes. |
| **Domain** | A hostname routed to `(stage, service, port)`, or a redirect to another hostname. |

### 5.2 Naming

| Object | Name |
|---|---|
| Compose project (data) | `dw-<app>-<stage>-data` |
| Compose project (stateless) | `dw-<app>-<stage>-blue` / `dw-<app>-<stage>-green` |
| Compose project (Caddy) | `dokwalt-system` (container `dokwalt-caddy`) |
| Stage network | `dw-<app>-<stage>` (bridge, created by the daemon, `external` in rendered files; Caddy attached) |
| Named volume | `dw-<app>-<stage>-<volume>` (created by the daemon, `external` in rendered files) |
| Built image | `dokwalt/<app>-<service>:<first 12 hex of image ID>` |
| Labels on every app container | `dokwalt.app`, `dokwalt.stage`, `dokwalt.service`, `dokwalt.role` (`data`/`app`), `dokwalt.ports` (declared container ports), `dokwalt.color` (stateless), `dokwalt.release` (stateless), `dokwalt.oneshot=true` (jobs) |

Stateful services don't get the `dokwalt.release` label. Their definition has to stay identical across releases, or compose would recreate the database on every deploy.

### 5.3 SQLite schema

```sql
CREATE TABLE IF NOT EXISTS apps (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  pipeline INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS stages (
  id INTEGER PRIMARY KEY,
  app_id INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  name TEXT NOT NULL,                              -- 'production' | 'staging'
  active_color TEXT NOT NULL DEFAULT '',           -- 'blue' | 'green' | '' (never deployed / no stateless services)
  current_release INTEGER NOT NULL DEFAULT 0,      -- releases.version, 0 = none
  config_version INTEGER NOT NULL DEFAULT 0,
  UNIQUE(app_id, name)
);
CREATE TABLE IF NOT EXISTS releases (
  id INTEGER PRIMARY KEY,
  stage_id INTEGER NOT NULL REFERENCES stages(id) ON DELETE CASCADE,
  version INTEGER NOT NULL,
  status TEXT NOT NULL,          -- deploying | succeeded | failed | superseded
  images TEXT NOT NULL,          -- JSON {"web":{"ref":"dokwalt/blog-web:3f9c2a1b7d0e","id":"sha256:…"}}
  compose TEXT NOT NULL,         -- JSON: the user's normalized compose model, as sent by the CLI
  config_version INTEGER NOT NULL,
  color TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  git_sha TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  finished_at INTEGER,
  error TEXT NOT NULL DEFAULT '',
  UNIQUE(stage_id, version)
);
CREATE TABLE IF NOT EXISTS config (
  stage_id INTEGER NOT NULL REFERENCES stages(id) ON DELETE CASCADE,
  key TEXT NOT NULL,
  value BLOB NOT NULL,           -- nonce || XChaCha20-Poly1305 ciphertext
  PRIMARY KEY(stage_id, key)
);
CREATE TABLE IF NOT EXISTS config_versions (
  stage_id INTEGER NOT NULL REFERENCES stages(id) ON DELETE CASCADE,
  version INTEGER NOT NULL,
  snapshot BLOB NOT NULL,        -- encrypted JSON of all vars at this version
  created_at INTEGER NOT NULL,
  PRIMARY KEY(stage_id, version)
);
CREATE TABLE IF NOT EXISTS domains (
  id INTEGER PRIMARY KEY,
  stage_id INTEGER NOT NULL REFERENCES stages(id) ON DELETE CASCADE,
  hostname TEXT NOT NULL UNIQUE,
  service TEXT NOT NULL DEFAULT '',
  port INTEGER NOT NULL DEFAULT 0,
  redirect_to TEXT NOT NULL DEFAULT ''   -- non-empty => 308 to https://<redirect_to>{uri}
);
CREATE TABLE IF NOT EXISTS service_overrides (
  stage_id INTEGER NOT NULL REFERENCES stages(id) ON DELETE CASCADE,
  service TEXT NOT NULL,
  stateful INTEGER,              -- NULL = auto-detect
  health_path TEXT NOT NULL DEFAULT '',
  health_timeout_s INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(stage_id, service)
);
CREATE TABLE IF NOT EXISTS metrics_containers (
  ts INTEGER NOT NULL, stage_id INTEGER NOT NULL, service TEXT NOT NULL,
  cpu_pct REAL, mem_bytes INTEGER, net_rx INTEGER, net_tx INTEGER, blk_r INTEGER, blk_w INTEGER
);
CREATE INDEX IF NOT EXISTS metrics_containers_ts ON metrics_containers(stage_id, ts);
CREATE TABLE IF NOT EXISTS metrics_http (
  ts INTEGER NOT NULL, hostname TEXT NOT NULL, requests INTEGER, s2xx INTEGER, s3xx INTEGER,
  s4xx INTEGER, s5xx INTEGER, p50_ms REAL, p95_ms REAL, p99_ms REAL
);
CREATE INDEX IF NOT EXISTS metrics_http_ts ON metrics_http(hostname, ts);
CREATE TABLE IF NOT EXISTS metrics_host (
  ts INTEGER NOT NULL, cpu_pct REAL, mem_used INTEGER, mem_total INTEGER,
  disk_used INTEGER, disk_total INTEGER, load1 REAL, temp_c REAL
);
CREATE INDEX IF NOT EXISTS metrics_host_ts ON metrics_host(ts);
CREATE TABLE IF NOT EXISTS alert_channels (id INTEGER PRIMARY KEY, kind TEXT NOT NULL, url TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS alert_state (key TEXT PRIMARY KEY, firing INTEGER NOT NULL, last_sent INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

Notes:
- `metrics_*.ts` is the start of the minute (unix seconds). Rows older than `metrics_retention_days` are deleted hourly.
- `settings` keys:

  | Key | Meaning | Default | Set by |
  |---|---|---|---|
  | `acme_email` | Let's Encrypt account email | — | `server init --email`, `POST /v1/settings` |
  | `keep_releases` | Succeeded releases per stage whose images are kept | `5` | `POST /v1/settings` |
  | `drain` | Delay before the old color is stopped | `10s` | `POST /v1/settings` |
  | `metrics_retention_days` | History kept | `7` | `POST /v1/settings` |
  | `alert.disk`, `alert.memory`, `alert.temperature`, `alert.restarts` | Alert thresholds | 90, 90, 80, 3 | `alerts:set` |
  | `check_token` | Random token served by Caddy for the domain check | random | daemon |
  | `stopped/<stage_id>` | `1` while a stage is stopped with `dokwalt stop` | — | `stop` / `start` |
  | `backup_endpoint`, `backup_region`, `backup_bucket`, `backup_prefix`, `backup_access_key` | Off-site backup storage (§7.7.1) | —, `auto`, —, `dokwalt/<hostname>`, — | `backup:setup` |
  | `backup_secret_key` | S3 secret key, encrypted with `secret.key` (base64) | — | `backup:setup` |
  | `backup_time`, `backup_keep_daily`, `backup_keep_weekly` | Daily time (server local), retention | `03:00`, `7`, `4` | `backup:setup` |
  | `backup_last_day`, `backup_last` | Date of the last scheduled attempt; outcome of the last run (JSON) | — | daemon |

- `alert_state.key` identifies a condition: `site:<host>`, `cert:<host>`, `deploy:<app>/<stage>`, `restarts:<app>/<stage>/<service>`, `oom:<app>/<stage>/<service>`, `reconcile:<app>/<stage>`, `backup`, `host:disk`, `host:memory`, `host:temperature`, `host:throttled`.

### 5.4 Release lifecycle

```mermaid
stateDiagram-v2
  [*] --> deploying: validated, release row created
  deploying --> succeeded: traffic switched
  deploying --> failed: error before or during the switch / daemon restarted mid-deploy
  succeeded --> superseded: a newer release succeeds
```

The model is validated (and images checked) *before* the release row exists, so an invalid compose file doesn't consume a version number. Mutating operations (deploy, rollback, promote, config releases, stop/start/restart, destroy) are serialised by one server-wide lock: a second operation waits for the first. The reconciler never waits; it skips a pass while an operation is running. `stages.current_release` always points to a `succeeded` release.

### 5.5 Config versions

`stages.config_version` starts at 0 and is displayed as `c<N>` (`c6`) to tell it apart from release numbers (`v8`). `config:set`, `config:unset`, `config:import` and `rollback --with-config` write the new `config` rows and a new snapshot in `config_versions`. A release records the config version it ran with, which is what `rollback --with-config` restores.

---

## 6. App definition

### 6.1 Sample app (unmodified repository)

```text
blog/
├── Dockerfile
├── docker-compose.yml
├── .env            # local dev only, git-ignored
└── src/…
```

```yaml
# docker-compose.yml — used as-is for local dev AND by DokWalt
services:
  migrate:
    build: .
    command: ["./bin/migrate", "up"]
    environment:
      DATABASE_URL: postgres://blog:${POSTGRES_PASSWORD}@db:5432/blog
    depends_on:
      db: { condition: service_healthy }

  web:
    build: .
    command: ["./bin/server"]
    ports: ["3000:3000"]
    env_file: .env
    environment:
      DATABASE_URL: postgres://blog:${POSTGRES_PASSWORD}@db:5432/blog
      PORT: "3000"
    depends_on:
      migrate: { condition: service_completed_successfully }
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:3000/healthz"]
      interval: 5s
      retries: 5

  worker:
    build: .
    command: ["./bin/worker"]
    env_file: .env
    environment:
      DATABASE_URL: postgres://blog:${POSTGRES_PASSWORD}@db:5432/blog
    depends_on:
      migrate: { condition: service_completed_successfully }

  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: blog
      POSTGRES_DB: blog
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes:
      - pgdata:/var/lib/postgresql/data
    ports: ["5432:5432"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U blog"]
      interval: 5s

volumes:
  pgdata:
```

### 6.2 What DokWalt infers

`dokwalt deploy` runs `docker compose config --no-interpolate --format json` on the Mac (with `-f` files if given). Compose normalises `extends`, profiles, short syntax and relative paths, but `${VAR}` stays literal, so the Mac's `.env` never leaks into production. The CLI sends this model to the daemon unchanged. It is stored as-is in the release, and the daemon transforms it for the target stage at deploy time.

| Service | Image | Kind (reason) | Readiness | Public | Project |
|---|---|---|---|---|---|
| `migrate` | built on the Mac | **one-shot job** (`web` and `worker` depend on it with `service_completed_successfully`) | exits 0 | never | `dw-blog-production-{blue,green}` |
| `web` | built on the Mac (same image as `migrate`) | stateless | compose `healthcheck` healthy, then HTTP check on its domain port | after `domains:add` | `dw-blog-production-{blue,green}` |
| `worker` | built on the Mac | stateless | running | no | `dw-blog-production-{blue,green}` |
| `db` | `postgres:17-alpine`, pulled on the server if absent | **stateful** (named volume `pgdata`) | compose `healthcheck` healthy | never | `dw-blog-production-data` |

Warnings in the deploy output:

```text
⚠ db: `ports` ignored — only the proxy is public (domains:add, db:connect)
⚠ web: `ports` ignored — only the proxy is public (domains:add, db:connect)
⚠ web: env_file ignored — config lives on the server; import it with `dokwalt config:import .env`
⚠ worker: env_file ignored — config lives on the server; import it with `dokwalt config:import .env`
⚠ ${POSTGRES_PASSWORD} is used in the compose file but not set — `dokwalt config:set POSTGRES_PASSWORD=...`
```

### 6.3 Transformation rules (daemon, at deploy time)

| Compose key | Rule | Severity |
|---|---|---|
| `build` | Removed; `image` set to the uploaded `dokwalt/<app>-<svc>:<id12>` | — |
| `image` without `build` | Kept; `pull_policy: missing` (pulled on the server if absent, not pinned by digest) | — |
| `ports` | Removed. The container ports are remembered as defaults for `domains:add`/`db:connect` | warn |
| `expose` | Kept; its ports also count as declared ports | — |
| `container_name` | Removed (would collide between blue and green) | warn |
| `env_file` | Removed (suggests `config:import`) | warn |
| `links`, `external_links` | Removed (services reach each other by name) | warn |
| `network_mode` other than `bridge` | **Rejected** | error |
| `secrets`, `configs` (service level) | **Rejected** (use `config:set`) | error |
| Bind mount whose source is inside the project folder (e.g. `./initdb:/…`) | **Rejected**: the server has no copy of the repository; bake it into the image or use a named volume | error |
| Bind mount with any other absolute host path | Allowed; the path must exist on the server | warn |
| Named volume | Top-level definition replaced by an external volume `dw-<app>-<stage>-<vol>` that the daemon creates. Shared by the data, blue and green projects. `external: true` volumes are kept as-is | — |
| `networks` | Replaced by the single stage network `dw-<app>-<stage>`, with the service name as alias | — |
| `x-dokwalt.uses: [<app>…]` (top level) | Shared services: the stateful containers of those apps join this stage's network (§11.5). Rejected if an app uses itself or a used app name is also one of its services; unknown `x-dokwalt` keys warn | error / warn |
| `restart` | Default `unless-stopped` (`no` for one-shot jobs) if unset | — |
| `logging` | Default `driver: local`, `max-size: 10m`, `max-file: 3` if unset | — |
| `labels` | User labels kept; `dokwalt.*` labels set (and overwritten) by DokWalt | — |
| `depends_on` | Kept within a project; entries crossing the data/color split are **dropped in both directions** (the data project is started and ready first) | — |
| `environment` | Kept. Stateless services also get every config key as a pass-through entry (`KEY: null`), which overrides a same-named `environment` entry | — |
| `healthcheck` | Kept (`disable: true` counts as none) | — |
| Anything else (`command`, `deploy.resources`, `mem_limit`, `user`, `cap_*`, …) | Kept verbatim | — |

**Stateful detection**, in order: (1) `services:set <svc> --stateful=…` override; (2) the service mounts a named volume; (3) the image's base name (registry, namespace and tag stripped), or a `<name>-*` variant of it, is one of `postgres`, `postgis`, `timescaledb`, `mysql`, `mariadb`, `percona`, `mongo`, `redis`, `valkey`, `keydb`, `dragonfly`, `memcached`, `rabbitmq`, `elasticsearch`, `opensearch`, `clickhouse`, `influxdb`, `minio`, `cassandra`, `couchdb`, `neo4j`, `nats`, `meilisearch`, `typesense`, `qdrant`; (4) otherwise stateless.

**One-shot jobs (release phase / migrations).** A service that another service depends on with `condition: service_completed_successfully` is a one-shot job:
- `restart: "no"` and the label `dokwalt.oneshot=true`;
- runs in the **new color** on **every deploy**, after the data project is ready and before its dependents start (compose orders it through `depends_on`);
- it must exit 0; a non-zero exit fails the deploy and the old version keeps serving. In practice `docker compose up` itself reports the failure (`service "migrate" didn't complete successfully: exit 1`) and the deploy error shows the tail of compose's output;
- the reconciler doesn't restart it, `restart` skips it, and it doesn't count toward crash-loop alerts.

This is DokWalt's release phase: the recommended pattern for database migrations, all in the compose file. Because the old color keeps serving until the switch, migrations must stay backward compatible (expand/contract).

**Missing variables.** `${VAR}` references with no default that aren't set in config are reported as warnings. Compose then substitutes an empty string.

### 6.4 Rendered projects

At deploy time the daemon renders the stored model into compose files under `/var/lib/dokwalt/apps/<app>/<stage>/`, one per project, named after it:

```json
// dw-blog-production-data.json
{
  "name": "dw-blog-production-data",
  "services": {
    "db": {
      "image": "postgres:17-alpine",
      "pull_policy": "missing",
      "environment": { "POSTGRES_DB": "blog", "POSTGRES_PASSWORD": "${POSTGRES_PASSWORD}",
                       "POSTGRES_USER": "blog" },
      "volumes": [{ "type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/data" }],
      "healthcheck": { "test": ["CMD-SHELL", "pg_isready -U blog"], "interval": "5s" },
      "restart": "unless-stopped",
      "logging": { "driver": "local", "options": { "max-file": "3", "max-size": "10m" } },
      "labels": { "dokwalt.app": "blog", "dokwalt.stage": "production",
                  "dokwalt.service": "db", "dokwalt.role": "data" },
      "networks": { "dokwalt": { "aliases": ["db"] } }
    }
  },
  "networks": { "dokwalt": { "name": "dw-blog-production", "external": true } },
  "volumes":  { "pgdata": { "name": "dw-blog-production-pgdata", "external": true } }
}
```

```json
// dw-blog-production-green.json (abridged)
{
  "name": "dw-blog-production-green",
  "services": {
    "migrate": {
      "image": "dokwalt/blog-migrate:3f9c2a1b7d0e",
      "pull_policy": "missing",
      "restart": "no",
      "environment": { "DATABASE_URL": "postgres://blog:${POSTGRES_PASSWORD}@db:5432/blog",
                       "POSTGRES_PASSWORD": null, "SESSION_SECRET": null },
      "labels": { "dokwalt.oneshot": "true", "dokwalt.role": "app", "dokwalt.color": "green",
                  "dokwalt.release": "8", "…": "…" }
    },
    "web": {
      "image": "dokwalt/blog-web:3f9c2a1b7d0e",
      "pull_policy": "missing",
      "command": ["./bin/server"],
      "depends_on": { "migrate": { "condition": "service_completed_successfully" } },
      "environment": { "DATABASE_URL": "postgres://blog:${POSTGRES_PASSWORD}@db:5432/blog",
                       "PORT": "3000", "POSTGRES_PASSWORD": null, "SESSION_SECRET": null },
      "healthcheck": { "…": "as in the compose file" },
      "labels": { "dokwalt.app": "blog", "dokwalt.stage": "production", "dokwalt.service": "web",
                  "dokwalt.role": "app", "dokwalt.color": "green", "dokwalt.release": "8" },
      "networks": { "dokwalt": { "aliases": ["web"] } },
      "restart": "unless-stopped",
      "logging": { "driver": "local", "options": { "max-file": "3", "max-size": "10m" } }
    },
    "worker": { "…": "same pattern" }
  },
  "networks": { "dokwalt": { "name": "dw-blog-production", "external": true } }
}
```

**Config values are never written to disk in clear text.** Config keys appear in stateless services only as pass-through entries (`KEY: null`), and `${VAR}` references stay literal. The daemon runs `docker compose` with the decrypted values in its process environment. Compose resolves pass-through entries and `${VAR}` interpolation (for all services, stateful included) from that environment. Built services from the same Dockerfile share one image ID, so `migrate`, `web` and `worker` above are uploaded once.

### 6.5 Completing the setup on the server

```bash
cd ~/Code/blog
dokwalt apps:create blog                # creates the app and links this folder (--no-link to skip)
dokwalt config:import .env.production   # or: dokwalt config:set POSTGRES_PASSWORD=… SESSION_SECRET=…
dokwalt deploy -m "First deploy"
dokwalt domains:add blog.example.com --service web --port 3000
dokwalt domains:redirect www.blog.example.com blog.example.com
dokwalt healthcheck:set --service web --path /healthz   # stricter HTTP check (2xx/3xx)
dokwalt services:set cache --stateful=false             # override a wrong detection
```

`domains:add` can run before the first deploy if `--service` is given. Until the app runs, Caddy answers 503 with a "not running yet" page.

### 6.6 Where server-side app state lives; export/import

App state is the rows in `/var/lib/dokwalt/dokwalt.db` (apps, stages, releases, config, domains, overrides). The rest lives alongside:

| What | Where | If lost |
|---|---|---|
| Rendered compose files | `/var/lib/dokwalt/apps/` | Regenerated at every deploy and reconcile |
| App data | Docker volumes `dw-<app>-<stage>-*` | Gone — back them up |
| Built images | `dokwalt/<app>-*` | Rebuilt by redeploying |
| Certificates | Docker volume `dokwalt-caddy-data` | Re-issued, which counts against rate limits |

`dokwalt apps:export [file]` prints (or writes to `file`, mode 0600) a JSON document:

```json
{
  "format": 1,
  "name": "blog",
  "pipeline": false,
  "stages": {
    "production": {
      "config": { "POSTGRES_PASSWORD": "…", "SESSION_SECRET": "…" },
      "domains": [ { "hostname": "blog.example.com", "service": "web", "port": 3000 },
                   { "hostname": "www.blog.example.com", "redirect_to": "blog.example.com" } ],
      "overrides": [ { "service": "web", "health_path": "/healthz" } ]
    }
  }
}
```

Config is exported **in clear text** (warning printed). Images, releases, metrics and volume data are not included. `dokwalt apps:import <file>` recreates the app, its stages, config, domains and overrides on the current server. Then run `dokwalt deploy` from the source folder and restore volume data separately (§19.1.7).

---

## 7. CLI command reference

### 7.1 Conventions

**Global flags**

| Flag | Meaning |
|---|---|
| `-a, --app <name>` | Target app. Resolution: flag → folder link (cwd or nearest parent recorded by `link`/`apps:create`) → error |
| `-s, --stage <name>` | `production` (default) or `staging`. Only accepted when the app has a pipeline |
| `--server <context>` | Target server. Resolution: flag → server recorded with the folder link → `current` |
| `--json` | Machine-readable output on read commands |

**Environment (Mac).** `DOKWALT_CONFIG` (path of `config.json`), `DOKWALT_SSH_CONFIG` (ssh config file used for `ssh -G`), `SSH_AUTH_SOCK` (agent), `PAGER` (for `docs`).

**Output.** On a TTY: colors, spinners, tables. Otherwise: no ANSI codes, no spinners. Errors say what happened and, where possible, what to run next.

**Stage display.** For apps without a pipeline, the stage never appears in output.

**Confirmations.** `apps:destroy` and `pipeline:disable` require typing the app name, or `--confirm <name>` for scripts. The dashboard asks before rollback, promote and restart.

### 7.2 Servers

#### `dokwalt server init <user@host> [--name <ctx>] [--email <addr>] [--binary <path>]`

Installs DokWalt on a server and saves it as a context (default name: the host's first label, or `default` for an IP). It becomes the current server if none is set yet. `--email` sets the Let's Encrypt account email. `--binary` uploads a local linux build. Otherwise the binary comes from next to the CLI (`dokwalt_linux_<arch>`, `dist/`) or is downloaded from GitHub releases for the CLI's version, with its SHA-256 checked against `checksums.txt`.

```text
$ dokwalt server init dd@pi.home --name pi --email dd@example.com
◆ Installing DokWalt on dd@pi.home
✓ Connecting over SSH
✓ Uploading dokwalt for linux/arm64 (14.8 MB)
ℹ Running the installer with sudo on the server (you may be asked for your password)
[sudo] password for dd: ••••••••
◆ Installing DokWalt  arm64
→ Installing Docker (official get.docker.com script)
✓ Docker 28.4.0
✓ Docker Compose 2.39.1
→ Configuring Docker: live-restore (containers survive Docker restarts), bounded logs
✓ Docker configured
✓ User dd can manage DokWalt (groups dokwalt, docker)
✓ Installed /usr/local/bin/dokwalt
→ Waiting for the daemon
✓ Daemon running (systemd unit dokwalt.service, starts at boot)
✓ Let's Encrypt account email: dd@example.com

✓ dd@pi.home is ready — saved as server pi

  Next, in your project folder:
    dokwalt apps:create myapp
    dokwalt deploy
    dokwalt domains:add myapp.example.com --service web
```

#### `dokwalt server add <name> <user@host>` · `server list` · `server use <name>` · `server remove <name>`

`add` registers an already installed server (e.g. from a second Mac). `list` shows contexts. `use` sets the current one. `remove` forgets a context locally and changes nothing on the server.

#### `dokwalt server info`

```text
$ dokwalt server info
◆ pi  dd@pi.home
  DokWalt     0.1.0 · linux/arm64 · up 12d
  Docker      28.4.0 · compose 2.39.1
  Host        4 CPUs · RAM 1.9 / 7.9 GB · disk 41 / 234 GB · 48 °C
  Daemon      27 MiB RSS (heap 9 MiB)
  Caddy       46 MiB RSS (heap 12 MiB)
  Apps        6 · domains 9
```

#### `dokwalt server upgrade [--binary <path>]`

Uploads this CLI's version for the server's arch and re-runs the idempotent bootstrap under sudo: replace the binary, update the unit if it changed, restart the daemon. Apps and Caddy keep serving: they're containers, and a daemon restart doesn't touch them. If the new version changes Caddy's compose definition, Caddy is recreated once (a ~1 s blip).

### 7.3 Apps

| Command | Description |
|---|---|
| `dokwalt apps` | List apps (and stages of pipeline apps) with status, release, domains |
| `dokwalt apps:create <name> [--no-link]` | Create an app with its `production` stage; links the current folder unless `--no-link` |
| `dokwalt apps:info` | Stages, services, domains, recent releases |
| `dokwalt apps:destroy <name> [--confirm <name>]` | Delete all containers, **volumes**, network, images, config, domains, releases |
| `dokwalt apps:export [file]` | Print or write (0600) the export document (§6.6) |
| `dokwalt apps:import <file>` | Create an app from an export |
| `dokwalt link <app>` · `dokwalt unlink` | Record / remove the folder → app link in the local config |

```text
$ dokwalt apps
  APP     STAGE        STATUS     RELEASE   DOMAINS
  blog                 running    v8        blog.example.com, www.blog.example.com
  shop    production   running    v31       shop.example.com
  shop    staging      running    v33       staging.shop.example.com
  notes                stopped    v4        notes.example.com
  api                  degraded   v12       api.example.com
```

Statuses: `running`, `degraded` (some containers not running or unhealthy), `down`, `stopped`, `deploying`, `not deployed`.

### 7.4 Deploy & releases

#### `dokwalt deploy [-m/--message <msg>] [--no-cache] [-f/--file <compose file>]…`

Builds on the Mac, uploads missing images and asks the daemon to deploy. The git SHA is captured automatically (`git rev-parse --short HEAD`, with `-dirty` if the tree has changes); `-m` defaults to the last commit subject. `-f` is repeatable, and defaults to compose's own (`compose.yaml` / `docker-compose.yml`).

```text
$ dokwalt deploy -m "New pricing page"
◆ Deploying blog  to pi (linux/arm64)
✓ Read compose file              4 services
✓ Built migrate, web, worker for linux/arm64                             38.4s
✓ Uploaded 16.6 MiB in 0.6s
⚠ web: `ports` ignored — only the proxy is public (domains:add, db:connect)
✓ Services — db: keeps its data (named volume pgdata), migrate: runs once per deploy, web: zero-downtime swap, worker: zero-downtime swap
✓ Created release v8
✓ Stateful services ready
✓ Starting v8 next to the current version · migrate exited 0 · containers running
✓ web is healthy                 GET / on port 3000
✓ v8 is live
✓ Previous version stops in 10s (in-flight requests finish)
✓ blog is live at https://blog.example.com
```

Failure: the new color is removed and the old one keeps serving.

```text
✓ Created release v9
✓ Stateful services ready
● Starting v9 next to the current version
→ Stopping the new version; the current version keeps serving
✗ docker compose up failed: exit status 1
  service "migrate" didn't complete successfully: exit 1
```

The daemon streams events as NDJSON (`application/x-ndjson`: `time`, `step`, `status`, `message`, optional `service`, `release`). The CLI renders them, as plain lines when stdout isn't a TTY.

#### `dokwalt ps`

```text
$ dokwalt ps
  SERVICE   CONTAINER                            STATE     HEALTH    RESTARTS   STATUS
  db        dw-blog-production-data-db-1         running   healthy   0          Up 12 days (healthy)
  web       dw-blog-production-green-web-1       running   healthy   0          Up 2 hours (healthy)
  worker    dw-blog-production-green-worker-1    running   –         0          Up 2 hours
```

#### `dokwalt restart [--service <svc>]` · `dokwalt stop` · `dokwalt start`

- `restart` restarts the active containers one at a time (`docker restart`), so replicas keep serving. One-shot jobs are skipped. No new release.
- `stop` runs `docker compose stop` on the data and active color projects and records `stopped/<stage_id>`. Domains then serve a 503 "temporarily unavailable for maintenance" page. The stage stays stopped across reboots: the reconciler, the site probes and the alerts leave it alone.
- `start` clears the flag, starts the containers and restores the routes.

#### `dokwalt releases [-n/--limit <n>]`

```text
$ dokwalt releases
  RELEASE   STATUS       DESCRIPTION          COMMIT    CONFIG   CREATED
  v9        failed       Add plans            b7e1f02   c6       2m ago
  ▶ v8      live         New pricing page     a1b2c3d   c6       2h ago
  v7        superseded   Set SESSION_SECRET   9f8e7d6   c6       1d ago
  v6        superseded   Fix RSS feed         9f8e7d6   c5       1d ago
```

Default limit 15.

#### `dokwalt rollback [vN] [--with-config]`

Without `vN`: the latest `succeeded`/`superseded` release older than the current one. It creates a new release (`Rollback to vN`) with `vN`'s images and compose model and the **current** config. `--with-config` first restores `vN`'s config, which creates a new config version. It goes through the same zero-downtime engine, with no build and no transfer. It fails with "image … is not on the server" if `vN`'s images were pruned (§9.3, step 9).

### 7.5 Config

| Command | Description |
|---|---|
| `dokwalt config [--reveal] [--shell]` | List vars. Values are masked (first 2 characters + `•`) unless `--reveal`; `--shell` prints `KEY="value"` lines in clear text |
| `dokwalt config:get <KEY>` | Print one raw value |
| `dokwalt config:set K=V... [--no-restart]` | Set vars → new config version → new release (zero-downtime) unless `--no-restart` or never deployed |
| `dokwalt config:unset K...` | Remove vars, same release behaviour |
| `dokwalt config:import <.env file>` | Import dotenv syntax (comments, `export `, quotes) |

Config values reach stateless services as environment variables, and all services through `${VAR}` interpolation. Stateful services only see values they reference as `${VAR}`. If such a value changes, the data project's definition changes, and compose recreates that service (e.g. the database) at the next release.

```text
$ dokwalt config
◆ Config of blog
  KEY                VALUE
  POSTGRES_PASSWORD  s3••••••••••••
  SESSION_SECRET     9f••••••••••••
  SMTP_HOST          sm••••••••••••
```

### 7.6 Domains & health checks

#### `dokwalt domains [--no-check]`

Lists domains with their target and a reachability check (§9.6); `--no-check` skips the probes.

#### `dokwalt domains:add <host> [--service <svc>] [--port <n>]`

- **Defaults.** `--service` defaults to the only stateless service that declares a port. Without `--port`, the port is automatic: resolved at each release from the service's first declared container port (`ports`/`expose`), otherwise 80 (with a warning).
- **Always added.** The domain is added and routed immediately. The CLI then prints the reachability result, with a warning if the domain doesn't reach the server yet. There's no confirmation prompt.
- **Local names.** Names ending in `.localhost`, `.test`, `.internal`, `.local`, `.lan`, `.home.arpa`, and bare IP addresses, get a certificate from Caddy's internal CA instead of Let's Encrypt.

```text
$ dokwalt domains:add blog.example.com --service web --port 3000
  blog.example.com → web:3000
✓ blog.example.com → reachable; HTTPS certificate is issued automatically
```

```text
$ dokwalt domains:add shop.example.com --service web
  shop.example.com → web:8080
⚠ shop.example.com was added but isn't reachable yet:
  DNS points to 198.51.100.7 but this server's proxy didn't answer on port 80 — check the IP,
  firewall (80/443) and router port forwarding. (Testing from the same LAN? Your router may not
  support hairpin NAT: then this check can't succeed but the site may still work from outside.)
  Caddy keeps retrying the certificate; check again with dokwalt domains
```

#### `dokwalt domains:remove <host>` · `dokwalt domains:redirect <from> <to>`

`redirect` adds `<from>` as a redirect-only domain: 308 to `https://<to>{uri}`, with its own certificate.

#### `dokwalt healthcheck:set --service <svc> --path <p> [--timeout 60s]` · `healthcheck:unset --service <svc>`

By default a public service passes the HTTP check with any answer below 500 on `/`. `healthcheck:set` makes it stricter: `GET <path>` must answer 2xx/3xx within `--timeout`. `unset` returns to the default. Both apply from the next release (no redeploy).

### 7.7 Services & runtime

#### `dokwalt services` · `dokwalt services:set <svc> --stateful=true|false`

```text
$ dokwalt services
  SERVICE   ON EACH DEPLOY                                                                WHY                                     HEALTH CHECK
  db        keeps its data (never duplicated; restarted only if its definition changes)   named volume pgdata                     default
  migrate   zero-downtime swap (a new copy starts, then replaces the old one)             no named volume, not a database image   default
  web       zero-downtime swap (a new copy starts, then replaces the old one)             no named volume, not a database image   /healthz
  worker    zero-downtime swap (a new copy starts, then replaces the old one)             no named volume, not a database image   default
  Override: dokwalt services:set <service> --stateful=false
```

`services:set` stores the override and redeploys the current release right away. Moving a service between the data and color projects recreates its container; named volumes are kept.

#### `dokwalt logs [-f] [--service <svc>] [--grep <text>] [-n/--lines <n>] [--since <dur>]`

Merged logs of the stage's active containers, one color per service. `--grep` is a case-insensitive substring filter applied on the server. `-n` defaults to 100. `--since` takes a duration (`30m`, `2h`). See §12.

#### `dokwalt exec [--service <svc>] [-- <cmd…>]`

Runs `docker exec -it` in the active container over an SSH PTY. The default service is the first stateless service that isn't a one-shot job; the default command is `sh`.

```bash
dokwalt exec                                  # shell in web
dokwalt exec --service web -- python manage.py migrate
```

#### `dokwalt db:backup [--service <svc>] [-o/--output <file>] [--database <name>]`

Postgres only. Resolves the target like `db:connect` (`GET …/db`, which also returns the container name), then runs `docker exec <container> pg_dump --format=custom --username=<user> --dbname=<db>` over an SSH session **without a PTY**, so the binary stream arrives intact, into `<file>.part`. pg_dump connects through the container's Unix socket (trusted by the official images), so no password is sent. The file is renamed to `<file>` (default `<database>-<stage>-<YYYYMMDD-HHMMSS>.dump`) only if pg_dump exited 0 and the file starts with the `PGDMP` magic.

#### `dokwalt db:connect [--service <svc>] [--tunnel-only] [--port <n>] [--remote-port <n>]`

- **Target.** Picks the database service (or `--service`; SQL databases are preferred over Redis-like ones) and detects its kind from the image (`postgres`/`postgis`/`timescaledb` → Postgres; `mysql`/`mariadb`/`percona` → MySQL; `mongo`; `redis`/`valkey`/`keydb`/`dragonfly` → Redis).
- **Credentials.** Read from the container's environment, e.g. `POSTGRES_USER`/`POSTGRES_PASSWORD`/`POSTGRES_DB`.
- **Tunnel.** Opens `127.0.0.1:<--port or random>` on the Mac, tunnelled through SSH with `dokwalt dial-stdio --tcp <container-ip>:<port>`.
- **Client.** Runs `psql`, `mysql`, `mongosh` or `redis-cli`. Passwords for psql, mysql and redis-cli are passed through the environment (`PGPASSWORD`, `MYSQL_PWD`, `REDISCLI_AUTH`), not as arguments; `mongosh` receives a `mongodb://user:pass@…` URL.
- `--tunnel-only` prints the connection URL and waits, for GUI tools. `--remote-port` overrides the detected container port.

```text
$ dokwalt db:connect --tunnel-only --port 5433
✓ Tunnel to db (postgres) open
  postgres://blog:••••@127.0.0.1:5433/blog
  Press Ctrl+C to close.
```

#### 7.7.1 Off-site backups

| Command | Description |
|---|---|
| `dokwalt backup:setup [--r2-account <id> \| --endpoint <url> --region <r>] [--bucket] [--access-key] [--secret-key-stdin] [--prefix] [--time HH:MM] [--keep-daily n] [--keep-weekly n]` | Store and check the storage settings (`POST /v1/backups/config`). Omitted fields keep their value |
| `dokwalt backup:disable` | Delete the `backup_*` settings (`DELETE /v1/backups/config`); objects stay in the bucket |
| `dokwalt backups [id]` | Setup, last run and complete backups (`GET /v1/backups`), or one backup's files (`GET /v1/backups/{id}`). `latest` = newest |
| `dokwalt backup:now` | Run a backup now (`POST /v1/backups/run`, NDJSON events) |
| `dokwalt backup:download <id> [file] [-o dir]` | Stream files through the daemon (`GET /v1/backups/{id}/files/{path}`), check SHA-256, write via `.part` + rename |
| `dokwalt backup:restore <id> [file] [--database name] [--confirm name]` | Restore into the originating service (`POST /v1/backups/{id}/restore`, NDJSON events) |

- **Storage.** Any S3-compatible API (Cloudflare R2 first), through `internal/s3`: Signature V4, path-style URLs, standard library only. Files above 100 MiB use multipart uploads (64 MiB parts; aborted on failure). Setup writes, lists and deletes `<prefix>/.dokwalt-test` before saving anything. The secret key never leaves the server again: downloads and restores go through the daemon.
- **Content.** One folder per backup, `<prefix>/<id>/`, `id` = start time in UTC (`20261010T030000Z`): `dokwalt.db` (`VACUUM INTO`, consistent while in use), and for each running Postgres service (stateful, Postgres image) of each deployed stage: `<app>-<stage>-<service>/globals.sql` (`pg_dumpall --globals-only`) and `<app>-<stage>-<service>/<db>.dump` (`pg_dump --format=custom`, checked for the `PGDMP` magic) for every database with `datallowconn` that isn't a template. Commands run with `docker exec` as `POSTGRES_USER` (default `postgres`) over the container's socket. `manifest.json` (paths, kinds, origin, sizes, SHA-256, errors) is uploaded last: a folder without it is an interrupted backup.
- **Disk.** Dumps are written one at a time to `/var/lib/dokwalt/backup-tmp`, hashed, uploaded and deleted.
- **Schedule.** `backupLoop` checks every minute; the backup is due once the daily time has passed and `backup_last_day` isn't today, so a server that was off catches up the same day. One attempt per day; `backup:setup` after the daily time doesn't trigger a run that day. One run at a time (a second gets 409).
- **Partial runs.** A service that should run but doesn't, or a database that fails to dump, is recorded in the manifest's `errors`; the rest is uploaded. Stopped stages are skipped. Storage errors fail the run.
- **Retention**, after complete runs only (a run with errors never deletes): keep the newest complete backup of each of the last `keep_daily` days that have one and of each of the last `keep_weekly` ISO weeks (overlapping, like restic), in server local time; delete the rest, manifest first. Interrupted folders older than 24 h are deleted. Folder names that aren't backup IDs are never touched.
- **Alerts.** Key `backup`: fires when a run fails or is partial, resolves after a complete one.
- **Restore.** The daemon downloads the file to `/var/lib/dokwalt`, checks its SHA-256, then runs in the originating service's container, as `POSTGRES_USER`: for a dump, `pg_restore --clean --if-exists -d <db>` if the database exists, else `pg_restore --create -d <maintenance db>` (no `--no-owner`: objects keep their owner, whose role must exist); for `globals.sql`, `psql` without `ON_ERROR_STOP` (existing roles are skipped with an error line). `dokwalt.db` is restored by hand (`dokwalt docs backups`).
- **Not encrypted client-side**: dumps rely on the provider's encryption at rest and on a bucket-scoped token; config values inside `dokwalt.db` stay encrypted with `secret.key`, which isn't uploaded.

### 7.8 Pipelines

| Command | Description |
|---|---|
| `dokwalt pipeline:enable` | Add a `staging` stage. Production is untouched |
| `dokwalt pipeline:disable [--confirm <name>]` | Delete staging's containers, volumes, network, config, domains, releases |
| `dokwalt promote` | New production release with staging's current images and compose model, and production's config. No build, no transfer |

```text
$ dokwalt promote
✓ Promoting staging v14 to production (same images, production config)
✓ Created release v9
✓ Starting v9 next to the current version · containers running · web is healthy
✓ v9 is live
```

### 7.9 Monitoring & alerts

| Command | Description |
|---|---|
| `dokwalt top` | Live CPU, memory, network and traffic of every app (refreshing view) |
| `dokwalt metrics [--since 24h]` | History as sparklines: CPU, memory, requests, latency, status codes |
| `dokwalt alerts` | Channels, thresholds, what is firing |
| `dokwalt alerts:add <discord\|slack> <webhook-url>` | Add a channel |
| `dokwalt alerts:remove <id>` | Remove a channel |
| `dokwalt alerts:test` | Send a test message to every channel |
| `dokwalt alerts:set key=value...` | Thresholds: `disk`, `memory` (% used), `temperature` (°C), `restarts` (per 10 min; `0` disables) |

```text
$ dokwalt metrics --since 24h
◆ blog · last 24h
  blog.example.com  req ▁▁▂▂▃▅▇█▆▅▄▃▃▂▂▁▁▁▂▃▄▃▂▁  1.9k · 5xx 0.2% · p50 21 ms · p95 84 ms · p99 240 ms
  web     CPU ▁▁▁▂▂▃▄▅▃▂▂▁▁▁▁▁▁▂▂▃▂▂▁▁  0.9%   MEM ▅▅▅▅▅▆▆▆▅▅▅▅▅▅▅▅▅▅▆▆▆▅▅▅  96 MB
  worker  CPU ▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁  0.2%   MEM ▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃▃  41 MB
  db      CPU ▁▁▁▂▂▂▃▃▂▂▁▁▁▁▁▁▁▂▂▂▂▁▁▁  0.3%   MEM ▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄  38 MB
```

### 7.10 Health & help

#### `dokwalt doctor`

Local checks (CLI version, local Docker) plus server checks from `GET /v1/doctor`: Docker and live-restore, memory cgroup, compose plugin, `docker`/`dokwalt` enabled at boot, disk, memory, temperature, throttling, clock (NTP), daemon RSS, Caddy (container, admin API, loaded config up to date), Let's Encrypt email, every stage (status, last failed release) and every domain (reachability).

```text
$ dokwalt doctor
✓ Versions               CLI 0.1.0, server 0.1.0
✓ Docker                 Docker 28.4.0 (API 1.51), 4 CPUs, 7.9 GB RAM
✓ Docker live-restore    containers keep running while dockerd restarts or upgrades
✓ Memory cgroup          cgroup 2 (systemd driver)
✓ Compose plugin         docker compose 2.39.1
✓ Start on boot: docker  docker starts at boot
✓ Start on boot: dokwalt dokwalt starts at boot
✓ Disk                   18% used (41 GB of 234 GB)
✓ Memory                 24% used (1.9 GB of 7.9 GB)
✓ Temperature            48°C
✓ Clock                  synchronized with NTP
✓ DokWalt daemon         build 0.1.0, 27 MiB RSS
✓ Proxy (Caddy)          running, config loaded
✗ api                    degraded: worker restarting
    → dokwalt logs -a api
⚠ Domain old.example.com DNS points to 198.51.100.7 but this server's proxy didn't answer on port 80 — …
```

#### `dokwalt docs [topic] [--search <words>] [--no-pager]` · `dokwalt version`

`docs` lists the embedded topics; `docs <topic>` renders one with Glamour in `$PAGER` (or prints it with `--no-pager` or when not on a TTY); `--search` finds topics containing the words. `version` prints the CLI build, API version and OS/arch.

---

## 8. TUI dashboard

`dokwalt` with no arguments on a TTY opens a full-screen bubbletea program connected to the current server (or `--server`). It refreshes the app list and live metrics (`GET /v1/top`) every 2 s. Without a TTY, `dokwalt` prints the help instead.

### 8.1 Layout

| Area | Content |
|---|---|
| Header | Server, host CPU / RAM / disk / temperature |
| App list (left) | Apps (staging rows under pipeline apps) with status and live CPU/RAM sparklines |
| Detail (right), tab 1 **Overview** | Services and containers, domains, current release |
| Tab 2 **Logs** | Live merged logs of the selected app |
| Tab 3 **Releases** | Release list; select one to roll back to it |
| Footer | Key hints and status messages |

### 8.2 Keybindings

| Key | Action |
|---|---|
| `↑↓` / `j k` | Select app (or release when the detail is focused) |
| `tab` | Focus list / detail |
| `1 2 3` / `← →` | Overview · Logs · Releases |
| `l` | Live logs |
| `r` | Roll back (to the selected release in Releases, otherwise the previous one) — asks `y` |
| `p` | Promote staging → production (pipeline apps) — asks `y` |
| `R` | Restart the app's containers — asks `y` |
| `o` | Open the site in the browser |
| `?` | Help |
| `q` / `ctrl+c` | Quit |

Deploys aren't triggered from the dashboard: they need a local build from a project folder.

### 8.3 Mockup

```text
 ◆ DokWalt · pi    CPU 12% ▂▃▂▂▅▃▂▂   RAM 1.9/7.9 GB   DISK 18%   48°C
╭───────────────────────────────╮╭──────────────────────────────────────────────────╮
│ ▸ blog      ● running   v8    ││ blog · v8 green · "New pricing page"               │
│   shop      ● running   v31   ││ [1 Overview]  2 Logs  3 Releases                   │
│    └ staging ● running  v33   ││                                                    │
│   api       ⚠ degraded  v12   ││ web     ● running  healthy   0.8%  96 MB  ▁▂▁▃▂▁   │
│   notes     ○ stopped   v4    ││ worker  ● running            0.3%  41 MB  ▁▁▁▁▁▁   │
│                               ││ db      ● running  healthy   0.1%  38 MB  stateful │
│                               ││                                                    │
│                               ││ https://blog.example.com → web:3000                │
╰───────────────────────────────╯╰──────────────────────────────────────────────────╯
 ↑↓ select · 1/2/3 tabs · r rollback · p promote · R restart · o open · ? help · q quit
```

During a rollback or promote started from the dashboard, the footer shows the engine's step events live.

---

## 9. Flows, step by step

Notation: **CLI** = on the Mac, **D** = daemon.

### 9.1 Server init

1. **CLI** resolves the target with `ssh -G` and connects (agent, then identity files; unknown host keys are confirmed or accepted per `StrictHostKeyChecking`).
2. **CLI** runs `uname -sm`: Linux only; `x86_64`/`amd64` → `amd64`, `aarch64`/`arm64` → `arm64`, anything else (32-bit Pi OS) is refused.
3. **CLI** picks the linux binary: `--binary`, a `dokwalt_linux_<arch>` next to the CLI or in `dist/`, or a download from GitHub releases for the CLI's version (checked against `checksums.txt`, cached). It uploads it to `/tmp/dokwalt-install-<random>`.
4. **CLI** runs `sudo <tmp> server bootstrap --user <ssh user>` (plus `--email`) in an SSH session **with a PTY**. The user types the sudo password on the server side; the CLI never sees it. Passwordless sudo or root skips the prompt.
5. **Bootstrap (root)**, idempotent:
   1. Requires Linux, root and systemd.
   2. Docker: if missing, installs it with the official `get.docker.com` script; enables `docker` at boot; installs `docker-compose-plugin` with apt if the compose plugin is missing.
   3. `/etc/docker/daemon.json`: merges `live-restore: true` and `log-driver: local` (the latter only if no log driver is set), keeping other keys. If it changed: `systemctl reload docker` (activates live-restore), then restart Docker.
   4. Warns if the memory cgroup is disabled (Pi: add `cgroup_enable=memory cgroup_memory=1` to `cmdline.txt`).
   5. Creates the group `dokwalt`; adds the user to `dokwalt` and `docker`.
   6. Installs the binary to `/usr/local/bin/dokwalt` (atomic rename); creates `/var/lib/dokwalt` (0750).
   7. Writes the systemd unit if it changed, `daemon-reload`, `enable`, `restart dokwalt`, and waits up to 90 s for `/v1/version` on the socket.
   8. Stores `acme_email` through `POST /v1/settings`.
6. **D** at first start creates `secret.key` and a random `check_token`, starts the Caddy project, and reconciles.
7. **CLI** saves the context, reconnects as the user (a fresh SSH connection, so the new groups apply) and prints next steps.

`server upgrade` repeats steps 1–5 with the CLI's version.

### 9.2 Deploy (first or subsequent)

1. **CLI** resolves app/server/stage and connects.
2. **CLI** runs `docker compose [-f …] config --no-interpolate --format json`.
3. **CLI** builds services with a `build` section: it writes a temporary compose file containing only those services' `build` sections, with `platform: linux/<arch>`, and runs `docker compose build` with `DOCKER_DEFAULT_PLATFORM=linux/<arch>` and `BUILDX_NO_DEFAULT_ATTESTATIONS=1` (plus `--no-cache`). It reads each image ID and tags it `dokwalt/<app>-<service>:<id12>`.
4. **CLI** calls `POST /v1/images/have` with the refs and streams the missing ones: `docker save --platform linux/<arch>` → zstd → `POST /v1/images/load`.
5. **CLI** calls `POST /v1/apps/{app}/stages/{stage}/deploy` with the model, image map, description, git SHA and project dir, and prints the NDJSON event stream.
6. **D** takes the server-wide operation lock, then:
   1. **Validate**: transform the model (§6.3) for this stage with the current config keys and overrides; reject errors; check every uploaded image exists; emit warnings (transformation, unset `${VAR}`, domains pointing to unknown services) and the service roles.
   2. **Release**: pick the new color (opposite of `active_color`; `blue` first; none if there are no stateless services) and insert release `vN` as `deploying` with the model, images, config version, description and git SHA.
   3. **Prepare**: ensure the Caddy project is running (§11.1); ensure network `dw-<app>-<stage>` and attach `dokwalt-caddy` to it; create the external volumes `dw-<app>-<stage>-<vol>`.
   4. **Data project**: write `dw-<app>-<stage>-data.json`; `docker compose up -d --remove-orphans` with config in the environment; wait until ready (§9.3.1, up to 3 min). If the model no longer has stateful services, the old data project is brought down (volumes kept).
   5. **New color**: write `dw-<app>-<stage>-<color>.json`; remove leftovers of that project if any; `docker compose up -d --remove-orphans`; wait until ready, which includes one-shot jobs exiting 0 (up to 3 min); run the HTTP health check of public services (§9.3.1).
   6. **Switch**: compute routes with the new color as active for this stage and apply the Caddy config (§11.3); record `active_color` and `current_release`.
   7. **Commit**: release `succeeded`, older `succeeded` releases → `superseded`, `deploy:<app>/<stage>` alert resolved.
   8. **Drain**: after `drain` (10 s), `docker compose down --remove-orphans --timeout 20` on the old color, unless it became active again in the meantime (e.g. a quick rollback). This runs in the background; the CLI returns after the switch.
   9. **Prune** (background): for each stage of the app, keep images referenced by the `keep_releases` (5) most recent succeeded/superseded releases, the current one included; remove other `dokwalt/<app>-*` tags. Registry images (e.g. `postgres`) are not pruned.
7. On any error in steps 6.3–6.6: `down` the new color (the old color keeps serving), mark the release `failed` with the error, fire a `Deploy failed` alert, and stream the last 30 log lines of the failing container. If the switch itself fails, the previous routes are re-applied.

Once the request is sent (step 5), the deploy belongs to the daemon: if the laptop disconnects, it runs to completion and `dokwalt releases` shows the outcome.

### 9.3 Zero downtime in detail

- The data project is updated in place: compose recreates a stateful container only if its definition changed. A code deploy never restarts the database, because stateful services carry no release label and aren't injected with config.
- Old and new colors run side by side between steps 6.5 and 6.8. On the stage network the alias `web` resolves to both. Workers of both colors run concurrently for ~10–15 s, so jobs must tolerate that.
- Caddy routes to container names (`dw-blog-production-green-web-1:3000`), load-balances across replicas with retries (`try_duration 5s`), and `POST /load` swaps configs gracefully.
- The e2e test sends continuous requests during a deploy: 0 failures out of ~4,000.

#### 9.3.1 Readiness and health gate

For every container of the project being started:

| Condition | Result |
|---|---|
| One-shot job exited 0 | ready |
| One-shot job exited non-zero | **fail** (`job <svc> failed with exit code N`) |
| Running, with no healthcheck or `healthy` | ready |
| Running, health `starting` | wait |
| `unhealthy` | **fail** |
| Exited / restarting / dead | **fail** (with "out of memory" if OOM-killed) |

When all are ready: a **3-second stability check**. Any restart or non-running (non-job) container → **fail** (`crash-looping`).

Then, for each **public stateless** service (it has a non-redirect domain), each container gets an HTTP probe `GET http://<container-ip on the stage network>:<domain port><path>`, polled every second until the timeout:

| | Path | Passes when | Timeout |
|---|---|---|---|
| Default | `/` | any status < 500 | 60 s |
| `healthcheck:set --path` | the path | 2xx or 3xx | `--timeout` (default 60 s) |

### 9.4 Config change

1. **CLI** `POST …/config {set, unset, no_restart}` (streams events).
2. **D** (operation lock): validates keys (`^[A-Za-z_][A-Za-z0-9_]*$`), writes encrypted values and a new config snapshot, bumps `stages.config_version`.
3. If the stage has a current release and `--no-restart` wasn't given: new release = current images + model, description `Set K1, K2` / `Unset K`, through the §9.2 engine from step 6.1. The data project is recreated only if a changed value is interpolated into a stateful service's definition.

### 9.5 Rollback

1. Target: `vN`, or the latest `succeeded`/`superseded` release older than the current one ("no earlier successful release" otherwise).
2. With `--with-config`: replace the config with `vN`'s snapshot (new config version).
3. New release `Rollback to vN` with `vN`'s images and model, through the §9.2 engine from step 6.1 (images must still exist on the server; no build, no transfer).

### 9.6 Add a domain

1. **CLI** `POST …/domains {hostname, service, port}`.
2. **D** lowercases the name and strips a trailing dot, then:
   - rejects wildcards and invalid names;
   - defaults the service from the current release (§7.6); `--service` is required before the first deploy. Without `--port`, the stored port is 0 = automatic, resolved from the service's declared ports at each release (80 if none, with a warning);
   - inserts the domain (hostnames are unique across the server, so a duplicate gets 409).
3. **D** applies routes. The new host gets a Let's Encrypt certificate, or an internal-CA one for local names (§11.2).
4. **D** runs the reachability check and returns it with the domain:
   - Local names → `ok (local name, internal certificate)`.
   - Otherwise `GET http://<host>/.well-known/dokwalt-check/probe` (8 s timeout). If the body equals the server's `check_token`, served by Caddy on `:80` for any host → `ok`.
   - Names are resolved through public resolvers (`1.1.1.1`, then `8.8.8.8`, queried directly, 2 s each), then the system resolver if none returns an address. Reason: the system resolver caches negative answers for the zone's SOA minimum, so a record created after `domains:add` stayed reported as missing long after it was live. The fallback keeps LAN-only names and hosts that block outbound DNS working.
   - Otherwise a DNS lookup (same resolution) explains: no record → "create an A/AAAA record"; a record → "DNS points to <ips> but this server's proxy didn't answer on port 80 — check IP, firewall, port forwarding (or hairpin NAT)".
5. **CLI** prints the target and the result; a failed check is a warning, and the domain stays configured. Caddy keeps retrying certificate issuance on its own schedule.

`dokwalt domains` re-runs the check for every domain.

### 9.7 Database connection

1. **CLI** `GET …/db?service=&port=` → D picks the service (given, or the first service with a recognised database image, SQL databases preferred over Redis-like ones), its kind, the container IP on the stage network, the port (`--remote-port`, the kind's default port, or the first declared port), and user/password/database from the container's environment.
2. **CLI** listens on `127.0.0.1:<--port or random>`. For each connection it opens an SSH session running `dokwalt dial-stdio --tcp <ip>:<port>` and copies bytes both ways.
3. Without `--tunnel-only`: runs the local client (password in its environment, except mongosh which gets a URL) and closes the tunnel when the client exits. With `--tunnel-only`: prints the URL and waits for Ctrl+C.
4. **Shared database.** If the stage has no database service of its own and no `--service` is given, but it uses other apps (`x-dokwalt.uses`), D resolves the first provider stage with a database service instead. User, password and database come from the consumer's connection URL whose host is one of the provider's aliases (config vars first, `DATABASE_URL` preferred, then the compose environment interpolated with config). Without one, D falls back to the provider's credentials and a database named after the consumer app. The response's `provider` field names the provider stage.

### 9.8 Remove app

`apps:destroy <name>` (confirmation or `--confirm <name>`). **D** under the operation lock, for each stage:
1. `down` the data, blue and green projects;
2. remove volumes labelled with the app and named `dw-<app>-<stage>-*`;
3. detach Caddy and remove the stage network;
4. delete the rendered files and the `stopped/` flag.

Then it deletes the app rows (cascade), removes `dokwalt/<app>-*` images and re-applies routes. The CLI removes local links to the app.

### 9.9 Enable pipeline

`POST /v1/apps/{app}/pipeline {"enabled": true}` → `apps.pipeline=1` and a `staging` stage row with empty config. Production isn't touched: no redeploy, no rename, because production objects already carry `production` in their names.

### 9.10 Promote

1. **D** requires staging to have a current release.
2. New production release: staging's images and model with production's config and overrides, description `Promote staging vN[: <description>]`, through the §9.2 engine from step 6.1. The model is re-transformed for production (names, volumes, domains). No build, no transfer.

### 9.11 Disable pipeline

`pipeline:disable` (confirmation or `--confirm`) → the stage destruction of §9.8 for `staging`, then `pipeline=0` and routes re-applied.

### 9.12 Stop / start

`stop`: set `stopped/<stage_id>=1`, apply routes (503 maintenance page), `docker compose stop` the data and active color projects. `start`: clear the flag, `docker compose start` both, apply routes.

---

## 10. Reboot & self-healing

### 10.1 Why sites break after reboots elsewhere, and DokWalt's answer

| Cause | DokWalt |
|---|---|
| The orchestrator is itself a container relying on Docker restart policies or Swarm state | Native systemd service, ordered after and requiring Docker |
| The proxy discovers backends dynamically and starts with an empty or partial view | Caddy boots from the last config file on disk; the daemon re-applies routes computed from SQLite and running containers |
| Containers left stopped or missing | `restart: unless-stopped`, plus the reconciler re-creates anything missing |
| A deploy half-finished at reboot time | Startup recovery marks it failed and tears down the unfinished color |
| An old color left running after an interrupted drain | Removed by the reconciler (never while still draining) |
| Docker restarts kill containers | `live-restore: true` |
| SD card corruption | NVMe recommended (§19); SQLite WAL |
| Wrong clock at boot → TLS errors | `doctor` clock check; time sync guidance |

### 10.2 systemd unit

Written by the bootstrap to `/etc/systemd/system/dokwalt.service`:

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

- `Requires=docker.service`: stopping Docker stops the daemon, and `Restart=always` brings it back.
- `HOME=/root`: the `docker compose` child process uses root's Docker config, so private registries work after `sudo docker login` on the server.
- `READY=1` is sent as soon as the API socket listens. The status then moves through `waiting for docker` → `recovering` → `running`.
- `WATCHDOG=1` is sent while the main loop's heartbeat is less than 10 minutes old, so a stuck main loop gets the daemon restarted.
- At start, the daemon disables transparent huge pages for itself (`prctl(PR_SET_THP_DISABLE)`) and re-execs, so the setting applies from the first allocation (§15).

### 10.3 Reconciler

Triggers: startup (after Docker answers, with backoff from 0.5 s to 10 s); every 60 s; and 3 s after Docker `die`, `oom` or `destroy` events for containers labelled `dokwalt.app` (bursts are coalesced). The events stream reconnects on its own.

```text
reconcile():
  if an operation holds the lock: skip this pass            # the deploy owns the state
  ensure the Caddy project runs (recreate only if not running or its compose file changed)
  for each stage with a current release:
      ensure network dw-<app>-<stage>; attach dokwalt-caddy if missing
      if stopped: continue
      re-transform the current release (current config and overrides)
      for the data project and the active color project:
          if any non-job service has no running/restarting container:
              create volumes; write the compose file; docker compose up -d --remove-orphans
      alert "Self-healing failed" on error / resolve it on success
  remove color projects of known stages that are neither active nor draining
  compute routes; if the desired Caddy config differs from the file or the loaded config: apply
```

Properties:
- It's cheap when nothing is wrong: a few Docker API calls, no compose invocation.
- It never removes volumes, and it only touches projects of stages it knows.
- A crash-looping container is "running" as far as Docker is concerned. The reconciler doesn't fight it; the restart-loop alert and `doctor` report it.
- Stopped stages stay stopped.

### 10.4 Startup recovery

Before the first reconcile, every release still `deploying` is marked `failed` ("interrupted (daemon restart or reboot during deploy)"), and its color project is brought down if it isn't the active color. The stage keeps its previous `current_release`.

### 10.5 Boot sequence

| Step | Event |
|---|---|
| 1 | systemd starts `docker.service`; containers with `restart: unless-stopped`/`always` start, **Caddy included, with its last config** |
| 2 | `dokwalt.service` starts; the API socket listens (`READY=1`) |
| 3 | The daemon waits for the Docker API, recovers interrupted deploys, reconciles |
| 4 | Sites answer over HTTPS with the persisted certificates as soon as their containers are up |

### 10.6 Acceptance tests (in `make e2e`)

The e2e server is a privileged Debian bookworm container running systemd, Docker and sshd (§18.6).
- **Reboot**: `docker restart` of the server container. The site must answer again with the latest release, with no command sent. The script prints how long it took.
- **Power cut**: `docker kill` of the server container, then start it. Same assertion.
- **Real hardware**: the Pi and VPS checklists (§19.1.8) add `sudo reboot` and pulling the plug.

---

## 11. Certificates & routing

### 11.1 Caddy project

The daemon writes `/var/lib/dokwalt/system/dokwalt-system.json` and runs it with `docker compose -p dokwalt-system up -d`:

```json
{
  "name": "dokwalt-system",
  "services": {
    "caddy": {
      "image": "caddy:2-alpine",
      "container_name": "dokwalt-caddy",
      "command": ["caddy", "run", "--config", "/config/caddy.json"],
      "restart": "always",
      "ports": ["80:80/tcp", "443:443/tcp", "443:443/udp"],
      "volumes": ["dokwalt-caddy-data:/data",
                  "/var/lib/dokwalt/caddy/config:/config",
                  "/var/lib/dokwalt/caddy/run:/run/caddy"],
      "labels": { "dokwalt.role": "system" },
      "logging": { "driver": "local", "options": { "max-size": "10m", "max-file": "3" } },
      "environment": { "GOMEMLIMIT": "96MiB", "GODEBUG": "disablethp=1" }
    }
  },
  "volumes": { "dokwalt-caddy-data": { "name": "dokwalt-caddy-data" } }
}
```

- The daemon recreates Caddy **only if it isn't running or this compose definition changed** (e.g. after an upgrade), which costs every site about a 1 s blip. Otherwise it leaves Caddy alone.
- Caddy is attached to every app network `dw-<app>-<stage>` (at deploy and by the reconciler), and detached when a stage is destroyed.
- Admin API: Unix socket `/run/caddy/admin.sock` in the container = `/var/lib/dokwalt/caddy/run/admin.sock` on the host. There's no TCP admin port.
- Logs to the daemon: Caddy's `net` writer on `unix//run/caddy/access.sock` (host `/var/lib/dokwalt/caddy/run/access.sock`, where the daemon listens), with `soft_start` so Caddy starts even when the daemon isn't listening yet.

### 11.2 Generated config (abridged)

```json
{
  "admin": { "listen": "unix//run/caddy/admin.sock" },
  "logging": { "logs": {
    "default": { "exclude": ["http.log.access.dokwalt"] },
    "dokwalt": {
      "writer":  { "output": "net", "address": "unix//run/caddy/access.sock", "soft_start": true, "dial_timeout": "2s" },
      "encoder": { "format": "json" },
      "include": ["http.log.access.dokwalt", "tls"]
    }
  }},
  "apps": {
    "http": { "servers": {
      "https": {
        "listen": [":443"],
        "logs": { "default_logger_name": "dokwalt" },
        "routes": [
          { "match": [{ "host": ["blog.example.com"] }], "terminal": true,
            "handle": [{ "handler": "reverse_proxy",
                         "upstreams": [{ "dial": "dw-blog-production-green-web-1:3000" }],
                         "load_balancing": { "selection_policy": { "policy": "round_robin" },
                                             "retries": 2, "try_duration": "5s" } }] },
          { "match": [{ "host": ["www.blog.example.com"] }], "terminal": true,
            "handle": [{ "handler": "static_response", "status_code": 308,
                         "headers": { "Location": ["https://blog.example.com{http.request.uri}"] } }] },
          { "match": [{ "host": ["notes.example.com"] }], "terminal": true,
            "handle": [{ "handler": "static_response", "status_code": 503,
                         "headers": { "Retry-After": ["30"] },
                         "body": "notes is temporarily unavailable for maintenance.\n" }] }
        ]
      },
      "http": {
        "listen": [":80"],
        "routes": [{ "match": [{ "path": ["/.well-known/dokwalt-check/*"] }], "terminal": true,
                     "handle": [{ "handler": "static_response", "status_code": 200, "body": "<check_token>" }] }]
      }
    }},
    "tls": { "automation": { "policies": [
      { "subjects": ["shop.localhost"], "issuers": [{ "module": "internal" }] },
      { "issuers": [{ "module": "acme", "email": "dd@example.com" }] }
    ]}}
  }
}
```

- Routes come from the domains table plus running containers: for each domain, the containers of its service in the active color (or in the data project, for a domain on a stateful service), `name:port`, one upstream per replica.
- Domains without running upstreams answer **503**: "`<app>` is not running yet." (not deployed, or containers down), or "`<app>` is temporarily unavailable for maintenance." (stopped).
- Redirect domains answer 308 to `https://<target>{uri}`.
- Caddy's automatic HTTPS adds the HTTP→HTTPS redirects and the ACME HTTP-01 handling on `:80`. The check route answers for any host.
- **Issuers**: names ending in `.localhost`, `.test`, `.internal`, `.local`, `.lan`, `.home.arpa` and IP addresses use Caddy's **internal CA**; everything else uses the ACME issuer with the account email (Let's Encrypt by default).
- HTTP/3 is served on 443/udp (Caddy default).

### 11.3 Applying the config

1. Compute routes; generate the config.
2. Write `caddy/config/caddy.json.tmp`, then rename it to `caddy.json`. The file is always complete, and Caddy uses it at its next boot.
3. `POST /load` over the admin socket. Caddy validates the config and swaps it in gracefully; on error the old config keeps running and the error is returned (a deploy then fails, and the previous routes are re-applied).
4. The reconciler compares a hash of the desired config with the file on disk and with the config Caddy reports (`GET /config/`), and re-applies on any difference, e.g. after Caddy restarted or containers got new names.

### 11.4 Certificates

- Stored in the Docker volume `dokwalt-caddy-data` (`/data` in the container), so they persist across reboots and Caddy recreation.
- Issued when a host first appears in the config, and renewed by Caddy before expiry. Caddy retries failed issuance with backoff and uses Let's Encrypt's staging endpoint for retries after a failure, which protects the production rate limits.
- Caddy's `tls` log events reach the daemon through the log socket. "certificate obtained" resolves, and an issuance error fires, the `cert:<host>` alert ("Could not get a certificate for <host>: … Run `dokwalt doctor` to check DNS and ports.").

### 11.5 Isolation through networks

There's no shared proxy network. Each app stage has a private bridge network `dw-<app>-<stage>` that its services join, with their service name as alias. Only `dokwalt-caddy` and the stateful containers of shared-service providers are attached to several networks. So an app can't reach another app's services, public or not, and Caddy reaches every service it routes to by container name.

**Shared services.** A stage whose compose file lists `x-dokwalt.uses: [<provider>…]` gets the provider's running stateful containers attached to its network, under the aliases `<provider>` (only when the provider has a single stateful service) and `<provider>-<service>`. The provider stage is the one with the consumer's stage name, else production.
- **Deploy.** Validation fails before a release is created if a provider is missing or not deployed. The rollout attaches the providers after the data project and before the new color starts, so one-shot jobs can reach them. After the switch, D re-syncs every attachment, because this stage may itself be a provider whose containers were just recreated.
- **Reconciler.** Each pass computes the desired attachments from every stage's current release, connects missing ones (fixing stale aliases), and disconnects provider containers from `dw-*` networks nobody wants them on. A recreated provider container loses its extra networks; compose leaves extra networks alone otherwise (checked with compose v2.39 and v5.6), so an unchanged provider never restarts.
- **Isolation.** Consumers share no network with each other; only the provider sits on several.
- `apps:destroy` refuses to delete an app other stages still use.

---

## 12. Logs

| Source | How | Stored |
|---|---|---|
| App containers | Docker `local` log driver, `max-size 10m`, `max-file 3` per container (set per service unless the compose file sets `logging`) | `/var/lib/docker/containers/…`, bounded |
| Deploy/rollback/promote progress | NDJSON event stream (`time`, `step`, `status`, `message`, `service`, `release`) | Only the error, in `releases.error` |
| Build output | Local on the Mac | Terminal |
| Daemon | stderr → journald (`journalctl -u dokwalt`), `log/slog` | journald |
| Caddy access logs | JSON over the Unix socket to the daemon, aggregated into metrics | **Never written to disk**; only `metrics_http` rows |
| Caddy errors | stderr → `local` driver | `docker logs dokwalt-caddy` |

**`dokwalt logs`** → `GET /v1/apps/{app}/stages/{stage}/logs?follow=1&service=&tail=&since=&grep=`. The daemon reads `ContainerLogs` of the stage's active containers (data project and active color), filters by service and by `grep` (case-insensitive substring, applied server-side to save bandwidth), and streams lines. The CLI prints `service | line`, with a stable color per service. `tail` defaults to 100; `since` (a duration) switches to "all lines since".

---

## 13. Metrics & alerts

### 13.1 Collection

| Metric | Source | Period |
|---|---|---|
| Container CPU, memory, block I/O | cgroup v2 files (`cpu.stat`, `memory.current`/`memory.stat`, `memory.max`, `io.stat`) in `/sys/fs/cgroup/system.slice/docker-<id>.scope` (or the `docker/` and `docker.slice/` layouts) | 10 s |
| Container network | `/proc/<pid>/net/dev` of the container's init process, excluding `lo` | 10 s |
| Fallback | Docker stats API when the cgroup files aren't readable | 10 s |
| Host CPU, memory, load, disk | `/proc/stat`, `/proc/meminfo`, `/proc/loadavg`, `statfs` | 10 s |
| Temperature | `/sys/class/thermal` | 10 s |
| Throttling (Pi) | `vcgencmd get_throttled` | 60 s |
| HTTP | Caddy access log JSON lines (host, status, duration) over the Unix socket, only for configured hosts | per request |
| Restarts, OOM | Docker events | event-driven |

### 13.2 Storage & retention

- **Live**: in memory, for `dokwalt top` and the dashboard (`GET /v1/top`).
- **History**: every minute the collector writes one row per (stage, service) to `metrics_containers`, one per host to `metrics_http` (requests, 2xx/3xx/4xx/5xx counts, p50/p95/p99 computed from per-minute histogram buckets), and one to `metrics_host`.
- **Retention**: `metrics_retention_days` (default 7), with a pruning pass every hour.
- **Queries**: `GET /v1/metrics` (server) and `GET …/metrics?since=` (app), rendered as sparklines by `dokwalt metrics`.

### 13.3 Alert rules

| Rule | Key | Threshold (`alerts:set`) | Fires | Resolves |
|---|---|---|---|---|
| Site down | `site:<host>` | always on | 2 consecutive failed probes, run every 60 s: connect to `127.0.0.1:443` with SNI = domain (no hairpin NAT needed), `GET /`, and fail on a connection error or a 5xx. Stopped, deploying and never-deployed stages and redirect domains are skipped | next successful probe |
| Deploy failed | `deploy:<app>/<stage>` | always on | a release fails | next successful deploy |
| Certificate error | `cert:<host>` | always on | Caddy tls event: could not obtain a certificate | certificate obtained |
| Crash loop | `restarts:<app>/<stage>/<svc>` | `restarts` (default 3) | ≥ N `die` events in 10 min (one-shot jobs excluded) | no death for 10 min |
| Out of memory | `oom:<app>/<stage>/<svc>` | always on | Docker `oom` event | — |
| Self-healing failed | `reconcile:<app>/<stage>` | always on | the reconciler can't restore a stage | next successful pass |
| Disk | `host:disk` | `disk` (default 90 %) | used ≥ threshold (checked every minute) | below threshold |
| Memory | `host:memory` | `memory` (default 90 %) | used ≥ threshold | below threshold |
| Temperature | `host:temperature` | `temperature` (default 80 °C) | ≥ threshold (hosts that report a temperature) | below threshold |
| Throttling | `host:throttled` | **always on** | `vcgencmd get_throttled` reports throttling or under-voltage *now* | cleared |

`alerts:set` accepts exactly `disk`, `memory`, `temperature` and `restarts`, all numeric. `0` disables a rule; otherwise it fires when the value is ≥ the threshold.

### 13.4 Delivery

- One alert per key per 30 minutes while it's firing: sent on the transition to firing, then again only after the 30-minute cooldown.
- When the condition clears, one "✅ Resolved: <title>" message.
- Discord: `{"content": "**🔴 <title>** — `<hostname>`\n<message>", "username": "DokWalt"}`. Slack: `{"text": "*🔴 <title>* — `<hostname>`\n<message>"}`. 10 s timeout. Failures are logged by the daemon.
- `alerts:test` posts "👋 DokWalt test alert" to every channel and reports per-channel errors.
- With no channel configured, alerts are only logged.
- Everything is evaluated on the server; nothing depends on the Mac being online.

```text
🔴 Site down — pi
blog.example.com is not answering: HTTP 502
```

---

## 14. Security model

### 14.1 Access control

- **Who can control DokWalt**: whoever can open `/run/dokwalt/dokwalt.sock` (root:dokwalt, 0660), i.e. root and members of `dokwalt`. `server init` adds the SSH user to `dokwalt` and `docker`. Remote access goes only through sshd.
- **Why no DokWalt password or token in v1**: the admin user is also in `docker`, which is root-equivalent (it can mount `/` into a container). Any secret DokWalt checked could be bypassed by that same user in one command. It would add prompts without adding a security boundary. A token will matter for a future restricted deploy user or a webhook (§17.2); the API can take an auth middleware then.
- **What actually protects the server**: SSH key-only auth, a passphrase-protected or hardware-backed key on the Mac, and known_hosts verification.
- **Risk to state plainly**: a compromised Mac with an unlocked agent means a compromised server.

### 14.2 Secrets

- **At rest**: config values and config snapshots are encrypted with XChaCha20-Poly1305 using `secret.key` (0600). This protects against leaks of `dokwalt.db` alone (a backup copied elsewhere, a DB attached to a bug report). It doesn't protect against root on the server, which can read the key and the containers' environment. Alert webhook URLs are stored unencrypted.
- **On disk**: rendered compose files contain config keys only as `KEY: null` pass-through entries and literal `${VAR}` references; values exist only in the environment of the `docker compose` process.
- **In output**: masked unless `--reveal`, `--shell` or `config:get`.
- **In containers**: environment variables, visible to `docker inspect` (root and the docker group only).
- **Backups**: `secret.key` must be backed up **separately** from `dokwalt.db`, and isn't part of off-site backups (§7.7.1); without it config can't be recovered. The off-site storage secret key is stored encrypted with it.
- **Export**: `apps:export` writes clear text (file mode 0600 + warning).

### 14.3 Network exposure

| Port | Listener | Exposure |
|---|---|---|
| 22/tcp | sshd | public (or restricted by firewall / VPN) |
| 80/tcp, 443/tcp, 443/udp | `dokwalt-caddy` | public |
| anything else | — | nothing is published |

- `ports:` are removed from every app service, so there are no Docker-published app ports. The Docker/ufw bypass (Docker's iptables rules are evaluated before ufw's) therefore can't expose an app or database. Only Caddy publishes, and 80/443 are meant to be public.
- Databases are only on their stage network; `db:connect` reaches them through SSH and `dial-stdio --tcp`.
- The daemon socket and the Caddy admin API are Unix sockets only.

### 14.4 Container isolation

- One private bridge network per app stage. An app can't reach another app's services (databases or web services), and staging can't reach production.
- Caddy is the only container attached to several networks.
- Compose security options are kept as written. DokWalt doesn't add seccomp/AppArmor profiles beyond Docker's defaults.

### 14.5 Supply chain & updates

- Release binaries are published with a SHA-256 `checksums.txt`. `server init`/`upgrade` verify downloaded server binaries against it.
- Docker comes from Docker's official install script; Caddy from the official `caddy:2-alpine` image.
- No telemetry and no update checks. The CLI contacts GitHub only to download a server binary.

### 14.6 Attack surface summary

| Surface | Mitigation |
|---|---|
| sshd | Keys only, no root login, optional rate limiting (docs) |
| Caddy (internet-facing) | Widely used; no host mounts besides its own config/run dirs and data volume |
| Apps | User code; private network per stage; no published ports |
| Daemon | No network listener; socket reachable by root and `dokwalt` only; systemd hardening options (§10.2) |

---

## 15. Resource budget

| Component | Target | Measured on the test server | How it's kept low |
|---|---|---|---|
| Daemon RSS | < 30 MB | ~27 MiB, of which ~9 MiB Go heap (the rest is shared binary code) | `GOMEMLIMIT=40MiB`; transparent huge pages disabled for the process (`prctl(PR_SET_THP_DISABLE)` + re-exec); 1 MiB SQLite cache; streaming everywhere |
| Daemon CPU idle | < 1 % | not yet benchmarked systematically | 10 s collector reading cgroup files, event-driven reconciler with a 60 s pass |
| Caddy RSS | originally 20–40 MB; revised to ≤ 60 MB | ~46 MiB, of which ~12 MiB heap | `GOMEMLIMIT=96MiB`, `GODEBUG=disablethp=1`, JSON config |
| **Total overhead** | **< 100 MB** | **~73 MiB** | — |
| Container logs | bounded | ≤ 3 × 10 MB per container | `local` driver |
| Images | bounded | 5 most recent successful releases per stage | pruning after each successful deploy |

Why THP matters: with transparent huge pages set to `always` (a common default), the kernel backs the Go heap with 2 MiB pages, which inflates the resident memory of a small Go process several times. `DOKWALT_THP=keep` turns off the daemon's opt-out for experiments.

**Measurement.** The daemon reports its own and Caddy's RSS and heap (`GET /v1/info`), shown by `dokwalt server info`. `doctor` shows the daemon's RSS. The numbers above come from the e2e server after a full run.

---

## 16. Failure modes & edge cases

| Scenario | Behaviour | User sees |
|---|---|---|
| Invalid compose (rejected key, relative bind) | Fails before the release is created | The rule and how to fix it |
| One-shot job fails | New color removed, release `failed`, alert | `service "<job>" didn't complete successfully: exit N` from compose |
| Health check or readiness fails / crash loop during the 3 s window | New color removed, old keeps serving, release `failed`, alert | Last 30 log lines of the failing container |
| Bad release that passes the health check | Stays live | Crash-loop / site-down alerts; `dokwalt rollback` |
| Caddy rejects the config at switch | New color removed, previous routes re-applied | "switch traffic: caddy rejected config: …" |
| Build fails on the Mac | Nothing sent | Build output |
| Registry image has no variant for the server arch / private registry without login | `up` fails → release `failed` | Compose error; hint: other image, `build:`, or `sudo docker login` on the server |
| Interrupted upload | No release created | Rerun `deploy`; images already on the server are skipped |
| Laptop disconnects after the deploy request | Daemon completes it | Outcome in `dokwalt releases` |
| Two operations at once | Second waits for the server-wide lock | Delay |
| Daemon crash or reboot mid-deploy | Startup recovery: release `failed`, unfinished color removed | Previous release live |
| Reboot during a drain | Reconciler removes the inactive color | Nothing |
| Docker restarts | `live-restore` keeps containers; events stream reconnects; reconcile | Nothing |
| Disk full | `disk` alert at 90 %; operations fail with Docker's errors | `doctor` ✗ Disk with a hint |
| OOM kill | `oom` alert; restart-loop alert if it repeats | Alert, `doctor` |
| DNS not ready when adding a domain | Domain added anyway; check explains; Caddy retries | Warning (§7.6) |
| Hairpin NAT at home | Check fails from the server itself | Warning mentions hairpin NAT |
| Let's Encrypt rate limit / validation failure | Caddy backs off and retries against staging | `cert` alert |
| Clock not synchronized | TLS failures | `doctor` warning with `timedatectl set-ntp true` |
| Ports 80/443 taken by another web server | Caddy can't start | `doctor` ✗ Proxy; `docker logs dokwalt-caddy` |
| `secret.key` lost | Config can't be decrypted | Restore it from backup (§19.1.7) |
| Config value interpolated into a stateful service changes | Data container recreated at the next release | Brief downtime of that service |
| Stateful image major upgrade (e.g. `postgres:16` → `17`) | Container recreated on old data; Postgres refuses to start → release fails, **the old DB container is already replaced** | Do major upgrades by dump/restore |
| Service removed from compose | Removed with the old color, or by `--remove-orphans` in the data project (volume kept) | — |
| Workers overlap during a deploy | By design, ~10–15 s | Jobs must be idempotent |
| Non-backward-compatible migration | Old color may error until the switch | Expand/contract migrations |

---

## 17. Extensibility

### 17.1 `dokwalt.*` labels

The `dokwalt.` label namespace belongs to DokWalt: it sets and overwrites these labels on every container, and uses them for reconciliation, routing and status. Structural hints are inferred or set server-side, so nothing in the repository is required. Labels will never carry config.

### 17.2 GitHub push-deploy (later)

- A webhook endpoint exposed through Caddy on a dedicated hostname, HMAC-verified: the first authenticated public endpoint.
- It queues a build job: shallow clone with a deploy key, then `docker compose build` on the server at low priority (systemd transient unit with `Nice=19`, `IOSchedulingClass=idle`, `MemoryMax=`).
- The resulting images feed the same engine entry point as the CLI (`Deploy(app, stage, {compose, images, description, git_sha})`). That's why the engine takes already-loaded images and a compose model, not build instructions.

### 17.3 Review apps (later)

Stages are already rows keyed by name, and naming (`dw-<app>-<stage>-*`) already accommodates more stages. A review app would be a stage `pr-123` with a generated domain (one certificate per host, backed by a wildcard DNS record) and a TTL.

### 17.4 Layer-aware image transfer (later)

A registry API (`/v2/`) served by the daemon through `dial-stdio`, backed by Docker's image store, so that only missing layers are sent from the Mac. The CLI's upload step (`images/have` + `images/load`) is isolated, so it can be swapped without touching the engine.

### 17.5 Multi-server

Contexts exist and each server is independent (its own DB and Caddy). Later: fan-out listing across servers and a server switcher in the dashboard. No cross-server scheduling.

### 17.6 API compatibility

The CLI and daemon speak `/v1` over the socket; `GET /v1/version` reports the daemon's build. `doctor` warns when the CLI and server versions differ, and `server upgrade` aligns them.

---

## 18. Milestones

M1–M4 are implemented in v0.1 (see Implementation status). Each milestone's acceptance criteria are covered by `make test` (unit) and `make e2e` (end-to-end, §18.6), marked (A), or checked by hand on real hardware, marked (M).

### M1 — MVP: single-environment apps that survive reboots — done

Scope: `server init/add/list/use/remove/info/upgrade`, bootstrap, systemd unit, Caddy project and config generation, `apps*`, `link/unlink`, compose transformation, build + zstd transfer, deploy engine (blue/green, data project, one-shot jobs, health gate, switch, drain, prune), `ps`, `restart/stop/start`, `releases`, `rollback`, `config*` with encryption, `domains*` with check, `healthcheck:*`, `services*`, `logs`, `exec`, `db:connect`, reconciler + startup recovery, `doctor`.

Acceptance:
- (A) Install on a fresh Debian bookworm server over SSH with `sudo`, then deploy the sample app and serve it over HTTPS (internal CA for `*.localhost` in the test).
- (A) Zero downtime: continuous requests during a deploy → 0 failures (~4,000 requests).
- (A) A config change creates a release and restarts with the new value.
- (A) Rollback returns to the previous release.
- (A) A failing deploy keeps the old version serving and marks the release failed.
- (A) `db:connect --tunnel-only` gives a working tunnel to the database.
- (A) Reboot (`docker restart` of the server) and power cut (`docker kill` + start): the site comes back with no manual action.
- (A) Unit tests: compose transformation (rules, one-shot detection, volume naming, pass-through config), Caddy config, store, alerts.
- (M) Same flow on a real Pi 5 and a 1 GB VPS, including `sudo reboot` and pulling the plug.

### M2 — Pipelines — done

Scope: `pipeline:enable/disable`, `-s/--stage` validation, `promote`, stage-aware output.

Acceptance:
- (A) Enabling a pipeline doesn't touch production; staging deploys independently; `promote` releases staging's images to production with production config, with no build or transfer.
- (A) `pipeline:disable` removes staging's containers, volumes, network, config, domains and releases.

### M3 — Monitoring & alerts — done

Scope: collector (cgroup v2 + Docker stats fallback), host metrics incl. temperature/throttling, Caddy access-log socket and HTTP aggregation, retention, `top`, `metrics`, Discord/Slack alerts with rate limiting and resolved messages, `alerts*`.

Acceptance:
- (A) Alert state machine: fire once, re-send only after 30 min, "resolved" on recovery (unit tests).
- (M) Real Discord/Slack webhooks receive `alerts:test` and a site-down alert.
- (M) Idle overhead within budget on the test server (§15).

### M4 — TUI, docs, open-source release — done

Scope: dashboard (§8), `--json` and non-TTY output, embedded docs (18 topics) with `docs --search`, README, release artifacts with checksums.

Acceptance:
- (A) Dashboard model tests.
- (M) Every docs topic renders; dashboard usable at 80×24.

### Next (post-0.1)

layer-aware image transfer (§17.4) · GitHub push-deploy (§17.2) · review apps (§17.3) · multi-server polish (§17.5) · DNS-01 · a benchmark script that enforces the resource budget automatically.

### 18.5 Repository layout

```text
cmd/dokwalt/            main.go
internal/cli/           cobra commands, output, dashboard (bubbletea), docs command, THP opt-out
internal/client/        typed API client over the SSH/dial-stdio transport
internal/sshx/          SSH: ssh -G resolution, agent/identity auth, known_hosts, PTY, uploads
internal/api/           shared request/response types
internal/daemon/        HTTP handlers, lifecycle, systemd notify/watchdog, doctor
internal/engine/        deploy/rollback/promote/config releases, health gate, drain, prune, reconciler
internal/compose/       transformation, stateful and one-shot detection, rendering
internal/proxy/         Caddy compose project, config generation, admin client
internal/metrics/       cgroup/proc readers, host metrics, HTTP access-log aggregation
internal/alerts/        rules, rate limiting, Discord/Slack
internal/store/         SQLite schema and queries
internal/secrets/       XChaCha20-Poly1305 box, key file
internal/docker/        minimal Docker Engine API client
internal/bootstrap/     server bootstrap/upgrade (runs as root)
internal/ui/            styles, spinners, tables
internal/docs/topics/   end-user Markdown, embedded and rendered by `dokwalt docs`
docs/                   SPEC.md, PROMPT.md
test/e2e/               run.sh, Dockerfile.server, sample app
Makefile                build, dist, test, lint, e2e, clean
LICENSE                 MIT
```

### 18.6 Build, distribution and tests

- `make build`: CLI for this machine plus `bin/dokwalt_linux_amd64` and `bin/dokwalt_linux_arm64` next to it (so `server init` finds them in development).
- `make dist`: release binaries for linux/amd64, linux/arm64, darwin/arm64 and darwin/amd64, plus `checksums.txt`, published on GitHub Releases. The version is embedded with `-ldflags -X …cli.Build=<git describe>`.
- Install: `curl -fsSL https://raw.githubusercontent.com/ddahan/dokwalt/main/install.sh | sh` (macOS/Linux/WSL; detects platform, verifies the checksum, picks a writable directory on `PATH`, sudo only if needed; re-run to upgrade). From source: `make build`. A Homebrew tap is planned. The server daemon is installed and updated only by `server init` / `server upgrade`, which upload the CLI's own version.
- `make test`: `go test ./...` (unit tests). `make lint`: `go vet` + `gofmt`.
- `make e2e` → `test/e2e/run.sh`: builds the binaries, starts a **privileged Debian bookworm container running systemd, Docker and sshd** as a throwaway server (`KEEP=1` keeps it), then drives the real CLI over SSH. It checks: install, deploy, zero downtime under load (0 failed requests out of ~4,000 during a deploy), config release, rollback, pipeline/promote, a failed deploy keeping the old version, the db tunnel, reboot recovery (`docker restart`) and power-cut recovery (`docker kill`). Everything runs locally; no paid service.
- Docs: `internal/docs/topics/*.md` is the single source for end-user docs (embedded with `go:embed`, readable on GitHub). This spec is contributor-facing.

---

## 19. Installation guides

The full end-user guides are the embedded topics **`internal/docs/topics/raspberry-pi.md`** (`dokwalt docs raspberry-pi`) and **`internal/docs/topics/vps.md`** (`dokwalt docs vps`). They are the source of truth for wording; this section specifies their required content, the justification of each recommendation, and the split between manual steps and what `dokwalt server init` automates.

### 19.1 Raspberry Pi 5

#### 19.1.1 Hardware

| Item | Recommendation | Why |
|---|---|---|
| Board | Pi 5 **8 GB** (4 GB acceptable) | Overhead ~60 MB; a small Node/Python/PHP site ~80–200 MB, Postgres ~40–100 MB. 4 GB ≈ 8–12 typical sites with DBs; 8 GB ≈ 20–30. RAM, not CPU, is the limit |
| Power | Official 27 W USB-C PSU | Under-voltage causes throttling, USB/NVMe resets and corruption; the Pi 5 needs 5 V/5 A for full USB/PCIe power |
| Cooling | Official Active Cooler | Sustained builds are on the Mac, but many containers + HTTPS keep the SoC busy; avoids throttling at 80–85 °C |
| Storage | **NVMe SSD on an M.2 HAT+** (256 GB+, e.g. a reputable 2230/2242 drive) | SD cards wear out under Docker layers, SQLite WAL and logs; a dying card is the classic "broken after reboot". NVMe is ~10× faster and far more durable |
| Boot from NVMe | EEPROM `BOOT_ORDER=0xf416` (NVMe first), via `raspi-config` → Advanced → Boot Order; `PCIE_PROBE=1` only for non-HAT+ adapters | Flash the OS directly onto the NVMe (USB enclosure on the Mac, or Imager running on the Pi from a temporary SD) |
| Optional | RTC battery (official ML-2020 rechargeable) | Correct time before NTP at boot → no TLS/log timestamp surprises after a power cut |
| Optional | UPS HAT | Rides through short cuts; with a graceful shutdown script it avoids unclean power loss entirely |

#### 19.1.2 OS choice

| | Raspberry Pi OS Lite 64-bit (**recommended**) | Ubuntu Server LTS arm64 (24.04 / 26.04) | DietPi |
|---|---|---|---|
| Pi 5 firmware/kernel support | First-party, fastest fixes (EEPROM, PCIe, `vcgencmd`) | Good, slightly behind | Based on Pi OS/Debian, good |
| Base | Debian 13 "trixie" (12 still fine) — Docker's `debian` repo | Ubuntu — Docker's `ubuntu` repo | Debian |
| Idle RAM | ~120 MB | ~250 MB (snapd, cloud-init) | ~60 MB |
| Support horizon | Follows Debian | 5 years standard | Rolling scripts |
| Surprises | Few | snapd, unattended cloud-init | Custom tooling (dietpi-*) differs from standard Debian guides |

Recommendation: **Raspberry Pi OS Lite (64-bit)**: first-party hardware support for the exact board, standard Debian (every guide applies), light. Flash with **Raspberry Pi Imager** → OS customisation: hostname (`pi`), user (not `pi`), **SSH public-key only**, locale/timezone, no Wi-Fi if on Ethernet.

#### 19.1.3 Steps and who does them

| # | Step | By | Justification |
|---|---|---|---|
| 1 | Assemble (HAT+, NVMe, cooler), flash Pi OS Lite 64-bit to NVMe with Imager customisation | hand | — |
| 2 | First boot, `sudo apt update && sudo apt full-upgrade -y`, `sudo rpi-eeprom-update -a`, reboot | hand | Current kernel/firmware fix PCIe/NVMe and power issues |
| 3 | Set boot order NVMe first (if not already booting from it) | hand | — |
| 4 | Check memory cgroup: `grep memory /sys/fs/cgroup/cgroup.controllers`; if absent append `cgroup_enable=memory cgroup_memory=1` to `/boot/firmware/cmdline.txt` (single line) and reboot | hand (bootstrap warns, doctor checks) | Needed for `mem_limit`, `docker stats`, DokWalt's per-container memory |
| 5 | zram swap (`sudo apt install zram-tools`, `ALGO=zstd`, `PERCENT=25`), disable `dphys-swapfile` | hand | Swap without disk writes; absorbs spikes instead of OOM kills |
| 6 | journald: `SystemMaxUse=100M`, `RuntimeMaxUse=50M` in `/etc/systemd/journald.conf.d/dokwalt.conf` | hand | Bounded log writes/disk |
| 7 | Time sync: `timedatectl` shows `System clock synchronized: yes` (systemd-timesyncd) | hand (doctor checks) | ACME and TLS fail with a wrong clock |
| 8 | Hardware watchdog: `/etc/systemd/system.conf.d/watchdog.conf` with `RuntimeWatchdogSec=15s`, `RebootWatchdogSec=2min` | hand | A frozen kernel/Pi reboots itself; then DokWalt reconciles |
| 9 | Power-loss behaviour: EEPROM `POWER_OFF_ON_HALT=0` (default), ext4 defaults, nothing else | hand (verify) | Pi boots automatically when power returns |
| 10 | Hardening (§19.1.6) | hand | — |
| 11 | Router: DHCP reservation, forward 80/tcp, 443/tcp, 443/udp to the Pi; DDNS if dynamic IP | hand | — |
| 12 | `dokwalt server init dd@pi.home --name pi --email you@example.com` | **automated**: Docker from get.docker.com (+ compose plugin), `daemon.json` merge (live-restore, `local` log driver), group `dokwalt` + your user in `dokwalt`/`docker`, binary, `/var/lib/dokwalt`, systemd unit; the daemon then creates `secret.key` and starts Caddy | — |
| 13 | `dokwalt doctor`, deploy a first app, final checklist (§19.1.8) | hand | — |

#### 19.1.4 Docker on ARM & images

- Docker Engine from Docker's official repo (what `get.docker.com` configures), never the distro's `docker.io` (older, different compose packaging).
- Builds on Apple Silicon for `linux/arm64` are native (no emulation), so fast; DokWalt sets `DOCKER_DEFAULT_PLATFORM=linux/arm64` automatically from the detected server arch.
- Registry images: check `docker manifest inspect <image> | grep arm64`. No arm64 variant → pick an alternative image, or build it from source via `build:` (arm64 native on the Mac). Don't run amd64 images under QEMU on the Pi (slow, fragile).

#### 19.1.5 Networking at home

- **Addressing**: DHCP reservation on the router (simpler and survives OS reinstall) rather than a static IP on the Pi.
- **Port forwarding**: 80/tcp (HTTP-01 challenge + redirects), 443/tcp, 443/udp (HTTP/3, optional).
- **Dynamic public IP**: router's built-in DDNS or `ddclient` updating the A record (e.g. Cloudflare API); keep TTL low (300 s).
- **CGNAT** (router WAN IP ≠ public IP seen by `curl ifconfig.me`, or in 100.64.0.0/10): ports can't be opened. Options: ask the ISP for a public IPv4 (often free); **Cloudflare Tunnel** (`cloudflared`, free); **Tailscale Funnel**. Implication: Let's Encrypt HTTP-01 needs the challenge request to reach Caddy's port 80, and visitors' TLS is terminated at the tunnel's edge. With Cloudflare Tunnel this works with two ingress rules per host: path `^/\.well-known/acme-challenge/` → `http://localhost:80` (Caddy answers the challenge; Let's Encrypt follows Cloudflare's HTTPS redirect), everything else → `https://localhost:443` with `originServerName: <host>` (Caddy's certificate secures the tunnel-to-origin hop). Tailscale Funnel only serves `*.ts.net` names, so it's for demos. The domain check warns about a DNS mismatch (DNS points at Cloudflare) — expected; the domain is added anyway. **v0.1 supports HTTP-01 only**; DNS-01 is open (§20).
- **IPv6**: add AAAA records only if the router allows inbound 80/443 to the Pi's global IPv6 address (many home routers block inbound IPv6 by default); a wrong AAAA record breaks HTTP-01 for IPv6-preferring validators. Caddy's published ports listen on IPv6 without Docker IPv6 networking.

#### 19.1.6 Security hardening

- Non-default user created by Imager; `/etc/ssh/sshd_config.d/10-dokwalt.conf`: `PasswordAuthentication no`, `KbdInteractiveAuthentication no`, `PermitRootLogin no`.
- `ufw default deny incoming; ufw allow 22/tcp; ufw allow 80/tcp; ufw allow 443/tcp; ufw allow 443/udp; ufw enable`.
- **Docker bypasses ufw** for published ports (its iptables rules are evaluated before ufw's). DokWalt's answer: no app or DB port is ever published (compose `ports:` stripped); the only published ports are Caddy's 80/443, which are public by intent. So ufw's policy and reality agree.
- `unattended-upgrades` with `Automatic-Reboot "true"` at `04:00`: safe because DokWalt restores every site after a reboot (that's the point of G1); security patches matter more than a 1-minute nightly-at-most blip.
- fail2ban costs ~40–60 MB (Python) — ~5 % of a 1 GB box; with key-only SSH brute force can't succeed, so it's optional. Lighter: OpenSSH ≥ 9.8 `PerSourcePenalties` (Debian 13+, Ubuntu 25.04+), or restrict SSH to Tailscale.
- Disable unused: `dtoverlay=disable-bt` and (on Ethernet) `dtoverlay=disable-wifi` in `/boot/firmware/config.txt`; `systemctl disable --now avahi-daemon bluetooth ModemManager cups` where present.
- DokWalt's own surface: no listening port; see §14.

#### 19.1.7 Pi health, backups, restore

- `vcgencmd measure_temp`, `vcgencmd get_throttled` (bits: 0 under-voltage now, 1 freq capped, 2 throttled, 3 soft temp limit; 16–19 = "has occurred since boot"). Temperature is shown in `server info`, `doctor`, `top` and the dashboard header; throttling in `doctor` and `top`; alerts `temperature` (threshold) and throttling (always on).
- **Back up**: `/var/lib/dokwalt/dokwalt.db` (online copy: `sudo sqlite3 /var/lib/dokwalt/dokwalt.db ".backup /backup/dokwalt.db"`), **`secret.key` (separately, offline)**, the Docker volume `dokwalt-caddy-data` (certificates; avoids re-issuance), Docker volumes `dw-*` (`docker run --rm -v dw-blog-production-pgdata:/v -v /backup:/b alpine tar czf /b/pgdata.tgz -C /v .` with the DB stopped, or logical dumps via `dokwalt exec --service db -- pg_dump …`). Images are not backed up (redeploy).
- **Restore on a fresh Pi**: prepare the Pi (steps 1–11), `server init`, `sudo systemctl stop dokwalt`, restore `dokwalt.db` (remove stale `-wal`/`-shm` files), `secret.key`, the `dokwalt-caddy-data` volume and the `dw-*` volumes, `sudo systemctl start dokwalt`. The daemon now knows every app, domain and config var, and Caddy serves the restored certificates. Built images aren't in the backup, so the reconciler can't start those apps yet ("Self-healing failed" alert): run `dokwalt deploy` once from each app folder. Alternative per app: `apps:export` / `apps:import` + `deploy` + volume restore.

#### 19.1.8 Final checklist

1. `dokwalt doctor` all ✓ (memory cgroup, clock, temperature, Caddy, domains).
2. `https://<domain>` from a phone on mobile data (not Wi-Fi: avoids hairpin illusions).
3. `dokwalt alerts:test` received on Discord/Slack.
4. **Hard reboot**: `ssh pi sudo reboot`; start a timer; site must be back over HTTPS < 2 min with no command; `dokwalt doctor` ✓.
5. **Power cut**: pull the power plug while serving traffic, wait 10 s, plug back; site back < 2 min; `doctor` ✓; `sudo sqlite3 /var/lib/dokwalt/dokwalt.db 'PRAGMA integrity_check'` = ok.
6. Optional watchdog test: `echo c | sudo tee /proc/sysrq-trigger` (kernel crash) → Pi reboots by itself within ~15 s + boot.
7. `vcgencmd get_throttled` = `0x0` after a day of normal load.
8. Backups scheduled and one restore rehearsed.

### 19.2 Debian/Ubuntu VPS — what differs

| Topic | VPS |
|---|---|
| OS | Debian 12/13 or Ubuntu 24.04/26.04 LTS minimal image |
| Size | 1 GB RAM runs ~4–6 small sites with DBs; add a 1–2 GB swap file (`fallocate`, `vm.swappiness=10`) — disk writes are not a concern on VPS storage |
| Storage/hardware/cooling/watchdog/EEPROM/`vcgencmd` | Not applicable (hypervisor handles it); temperature checks are skipped automatically |
| Networking | Public IP: no port forwarding, DDNS, CGNAT or hairpin concerns; IPv6 usually provided — add AAAA if the provider routes it |
| Firewall | Provider firewall/security group: allow 22, 80, 443/tcp, 443/udp; plus ufw as on the Pi (same Docker-bypass reasoning) |
| Users | Cloud-init creates the user with your key; ensure it's not root: create a user, add key, disable root login |
| Updates | `unattended-upgrades` with automatic reboot at a quiet hour |
| Backups | Provider snapshots (whole disk, simplest restore) + nightly off-site backups (§7.7.1) |
| Init | `dokwalt server init deploy@203.0.113.10 --name prod --email you@example.com` — identical |

---

## 20. Open questions

1. **Settings CLI.** *Resolved in v0.1:* `dokwalt server settings [key=value...]` shows and changes `acme_email`, `keep_releases` (5), `drain` (10s) and `metrics_retention_days` (7), with validation.
2. **Resource limits set server-side.** The brief lists them among server-side overrides; v0.1 only honours `deploy.resources` / `mem_limit` from the compose file. *Proposal:* `services:set <svc> --memory 256m --cpus 0.5` (two more columns in `service_overrides`).
3. **Schema migrations.** The schema is `CREATE TABLE IF NOT EXISTS`. The first incompatible change needs versioned migrations (`PRAGMA user_version`) and a pre-upgrade DB backup in `server upgrade`.
4. **Registry image pinning.** Registry images use `pull_policy: missing` and aren't pinned by digest, so a rollback reuses whatever image is present locally for that tag. *Proposal:* record the resolved image ID in the release and warn when it changed.
5. **DNS-01 / CGNAT.** HTTP-01 only. Cloudflare Tunnel works with the two-rule setup (§19.1.5). *Proposal:* Cloudflare DNS-01, which needs a Caddy image built with the DNS module.
6. **Export encryption.** `apps:export` writes config in clear text. *Proposal:* optional passphrase encryption.
7. **Alert webhook URLs at rest.** They're bearer secrets but stored unencrypted. *Proposal:* seal them with the same key as config.
8. **Certificate expiry alert.** v0.1 alerts on issuance errors reported by Caddy; there's no alert based on the served certificate's expiry date.
9. **Rollback/promote confirmation in the CLI.** The dashboard asks; the CLI commands run immediately. Confirm that this is wanted.
10. **CLI/daemon compatibility policy.** `doctor` warns on version mismatch; there's no hard API-version gate yet. *Proposal:* refuse on a major API mismatch.
11. **Install script / Homebrew tap.** *Install script resolved in v0.1* (`install.sh`); a Homebrew tap is still planned.
12. **`alerts:set … = 0`.** *Resolved in v0.1:* `0` disables any threshold rule.
13. **One-shot job logs on failure.** *Resolved in v0.1:* when `docker compose up` fails, the engine prints the last log lines of every one-shot job that exited non-zero, so a failing migration shows its own error.
14. **Out-of-memory alert resolution.** The `oom:` alert never sends "resolved"; it re-fires at most every 30 minutes.
15. **`--json` for operations.** Deploy/rollback/promote events are NDJSON on the API but the CLI has no JSON output for them.
