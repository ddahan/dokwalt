# DokWalt — Technical Specification Request

## Your task

Write a **technical specification document** for **DokWalt**, a lightweight, CLI-driven, self-hosted platform for deploying websites with Docker Compose on a single small server.

**Do not write implementation code.** Produce a spec I can review and approve before any code is written. Where my decisions below are technically weak or wrong, say so and propose a better alternative. If something is ambiguous enough to change the design, ask me before writing.

---

## Context & motivation

I host many websites (any stack) on a small VPS, and later on a Raspberry Pi. [Dokploy](https://dokploy.com/) does roughly what I need, but:

- It's bloated: a web UI and many features I don't use.
- It's built around Docker Swarm, clusters and multi-node setups, which I don't need.
- **When the server reboots, my websites are often broken.** This is the #1 pain point DokWalt must solve.

DokWalt should combine **Heroku's architecture principles** (apps, releases, config as env vars, pipelines with instant promotion, 12-factor) with **Dokploy's native Docker Compose support**, and nothing more.

---

## Guiding principles

1. **Small footprint first.** Many sites share one small machine. The platform's own overhead must be minimal in RAM, CPU and disk.
2. **Simple, boring, few moving parts.** No component unless it earns its place (e.g. no Redis; no Postgres for DokWalt's own state).
3. **Reliable by design.** Actual state must always converge back to desired state, especially after a reboot.
4. **CLI-only, and gorgeous.** No web UI. The terminal experience should be modern, polished, real-time and interactive.
5. **12-factor.** Config in env vars, logs to stdout, stateless processes, disposable containers, dev/prod parity.

---

## Decisions already made

### Language & distribution
- **Go.** Ship a single static binary, cross-compiled for `darwin/arm64` (my Mac), `linux/amd64` (VPS) and `linux/arm64` (Raspberry Pi).
- Use the same binary in two roles: `dokwalt` (CLI on my laptop) and `dokwalt daemon` (on the server). Justify or challenge this.

### Topology & access
- The CLI runs on **my Mac** and talks to the server daemon **over SSH** (e.g. to a Unix socket forwarded through SSH). The control plane is **never exposed on a public port**.
- Only I use it; there are no multiple users or roles. Access must be protected: SSH keys at minimum. Evaluate whether an extra DokWalt passphrase or token adds real value on top of SSH.
- Design server "contexts" (`dokwalt server add prod user@host`) so multiple servers are possible later. v1 only needs to handle one server well.
- Provide one-command server bootstrap (`dokwalt server init user@host`): install or check Docker, install the daemon, set up the systemd unit and reverse proxy, and verify.

### Builds
- **Builds happen on my Mac** (powerful Apple Silicon) with `docker buildx`, targeting the server's architecture (auto-detected).
- **No CI, no GitHub Actions, no paid services.**
- Transfer images to the server efficiently. Compare `docker save | zstd | ssh docker load` against a layer-aware approach (e.g. a temporary registry over an SSH tunnel so only changed layers are sent) and recommend one.
- Nothing is built on the server in v1.

### Docker Compose (native)
- An app is defined by its **existing `docker-compose.yml`**, which DokWalt should not require me to rewrite.
- **No DokWalt-specific file in the repository (strict 12-factor).** Anything that varies between deploys (env vars, secrets, domains, resource limits, stage settings) lives **only on the server**, managed through the CLI. The repo contains only the app's structure, which is already expressed by the Dockerfile and `docker-compose.yml`:
  - **Health checks** come from the compose file's native `healthcheck:`; `dokwalt healthcheck:set` can override or add one on the server.
  - **Stateful services** (databases, volumes) are detected automatically (named volumes, well-known database images), with a server-side override if detection is wrong.
  - **Public services and ports** are chosen when attaching a domain on the server (e.g. `dokwalt domains:add myapp example.com --service web --port 3000`).
  - If something truly structural can't be inferred, the spec may propose optional `dokwalt.*` labels in the compose file, but these must never contain config.
- DokWalt applies its own overrides (networks, labels, restart policies, env injection, log rotation, resource limits) without editing my compose file.
- Databases are **just services in the compose file**; there are no managed add-ons. DokWalt must still handle them correctly (see zero-downtime below).
- **Database access from my laptop:** `dokwalt db:connect myapp [--service db]` opens an SSH tunnel to a database service's port. Databases are **never exposed publicly**. The command should either launch the matching local client (`psql`, `mysql`, `redis-cli`, …) or just open the tunnel so I can use a GUI tool (`--tunnel-only`, printing the local port and connection URL). Detect the database type from the image where possible.

### Reverse proxy & certificates
- **Automatic Let's Encrypt certificates**, including renewal. My proposal is **Caddy** (built-in automatic HTTPS, admin API for dynamic config, low memory). Validate it against Traefik and alternatives, and justify the choice.
- Certificates and proxy config must persist and be re-applied automatically after a reboot.
- **Domains are explicit, per app and per stage**: I create a DNS record for each one; no wildcard domains or wildcard certificates. Before requesting a certificate, DokWalt should check that the domain resolves to the server and explain clearly if it doesn't, rather than burning Let's Encrypt rate limits on failing attempts.
- `dokwalt domains:add/remove/list myapp` manages domains, including extra domains and redirects (e.g. `www` → apex).

### State storage
- **SQLite** (WAL mode) for all DokWalt state: apps, stages, releases, config and metrics. No other datastore.

### Heroku-style concepts
- **App**: a compose project plus DokWalt config.
- **Release**: an immutable, numbered pair of *(image digests + config version)*. Every deploy or config change creates a new release (v1, v2, …).
- **Rollback**: `dokwalt rollback myapp [--to v12]` is instant, using retained images from the last N releases (configurable).
- **Pipelines are optional (opt-in per app).** Small projects shouldn't pay any complexity cost for them.
  - **By default, an app has a single environment.** Commands never ask for a stage, and nothing about stages shows up in the CLI output, TUI or config files for that app.
  - `dokwalt pipeline:enable myapp` adds a **staging** stage next to production. Both stages run on the **same server** with separate domains, env vars and containers. `pipeline:disable` removes it cleanly.
  - **Promotion** (pipeline apps only): `dokwalt promote myapp` takes the **exact images currently running in staging** and releases them to production with production's config. **No rebuild. It should be near-instant.**
  - The data model must support stages from day one, so that enabling a pipeline on an existing app needs no migration or redeploy of production.

### Zero-downtime deploys
- Start the new version, wait for the health check to pass, switch proxy traffic, drain, then stop the old version. If the health check fails, abort automatically and keep the old version live.
- Compose has no native blue/green, so the spec must explain concretely how this works. For example, stateless services get swapped (blue/green compose projects) while **stateful services (databases, volumes) are never duplicated** and live in a stable, separate project.

### Config / 12-factor
- `dokwalt config:set KEY=VALUE --app myapp`, `config:get`, `config:unset` and `config` (list). `--stage staging` is only accepted for apps with a pipeline enabled.
- Inside a project folder, `--app` can be omitted: `dokwalt link myapp` records the folder → app mapping in my **local user config** (like a Heroku git remote), so nothing is added to the repo.
- Config changes create a new release and trigger a zero-downtime restart.
- Propose how secrets are stored at rest (e.g. encrypted in SQLite) and never printed in plain text unless requested.

### Reboot resilience (critical)
- The daemon runs as a **systemd** service, ordered after Docker.
- **The daemon runs directly on the host, never in a container** (neither Docker-in-Docker nor a container with the Docker socket mounted). The component that repairs containers must not depend on Docker's restart policies to come back itself. A container would also add no isolation (socket access is root-equivalent) and would complicate access to host metrics, systemd, SSH and volume paths. Apps always run in containers. The reverse proxy may run either as a container supervised by the daemon or as a host service; justify the choice.
- On startup it **reconciles**: it compares desired state (SQLite) with actual Docker state, recreates or repairs anything missing, re-applies proxy config and verifies health checks. Don't rely only on Docker restart policies.
- Include `dokwalt doctor`, which diagnoses and explains problems (DNS not pointing to the server, certificate failures, unhealthy containers, low disk, and so on).
- Define an explicit **"hard reboot" acceptance test**: after a `sudo reboot`, all sites must be back up and serving HTTPS without any manual action.

### Logs
- **Real-time log streaming** (`dokwalt logs myapp -f`), multiplexed across services with colors per service, filterable by service or stage, with search or grep.
- Build and deploy logs are streamed live during `deploy` and `promote`.
- Bound disk usage with log rotation per container.

### Performance monitoring
- **Live** CPU, RAM, network and disk I/O per app and per service, htop-style in the TUI.
- **History** (7 days by default) stored in SQLite with downsampling, shown as sparklines or charts in the terminal.
- **HTTP metrics** per site (requests/sec, latency percentiles, status code breakdown), sourced from the reverse proxy. Keep collection itself cheap.
- **Alerts** via **Discord and/or Slack webhooks**: site down, container restart loop, certificate renewal failure, disk or RAM thresholds, failed deploy. Alert rules should be configurable and rate-limited.

### CLI / TUI experience
- Built with the Charm stack (Bubble Tea, Lip Gloss, Bubbles, Huh, Glamour) or a justified alternative.
- **Two modes:**
  1. **Scriptable commands** (`dokwalt apps`, `dokwalt deploy`, …) with clean non-TTY output and a `--json` flag.
  2. **Interactive full-screen dashboard** (`dokwalt` with no arguments, in the spirit of k9s or lazydocker) to browse apps, see live status and metrics, tail logs, and trigger deploy, promote or rollback with confirmations.
- Live progress for long operations (build → transfer → start → health check → switch), with clear errors and suggested fixes.
- **In-app documentation**: `dokwalt docs [topic]` renders embedded, searchable Markdown in the terminal (concepts, guides, troubleshooting). Every `--help` includes real examples.

---

## Raspberry Pi 5 installation guide

The spec must include a complete, step-by-step **guide to installing DokWalt on a Raspberry Pi 5**. Write it for someone comfortable in a terminal but not a Linux sysadmin. It will ship as part of the docs (`dokwalt docs raspberry-pi`). Justify every recommendation, and say which steps `dokwalt server init` automates and which I do by hand.

Cover at least:

- **Hardware:**
  - RAM model: 4 GB vs 8 GB, and roughly how many typical sites each can host.
  - Power and cooling: the official 27 W power supply and an active cooler.
  - **Storage: an NVMe SSD (via an M.2 HAT) strongly preferred over an SD card.** Docker, SQLite and logs wear SD cards out, and a dying card is a classic cause of "broken after reboot". Explain how to boot from NVMe.
  - Optional extras: a UPS HAT and the RTC battery.
- **OS choice:**
  - Compare Raspberry Pi OS Lite (64-bit), Ubuntu Server LTS (arm64) and a minimal option such as DietPi, then recommend one. Headless, no desktop.
  - Flashing with Raspberry Pi Imager, with the SSH key, hostname and user preconfigured.
- **System configuration:**
  - Enable the memory cgroup if the OS doesn't by default. It's required for container memory limits and `docker stats`.
  - Swap or zram.
  - Reduce disk writes (journald limits, log rotation).
  - Reliable time sync (TLS fails with a wrong clock).
  - Firmware/EEPROM updates.
  - The **hardware watchdog**, so a frozen Pi reboots itself.
  - Make sure the Pi recovers cleanly after a power cut.
- **Docker on ARM:**
  - Install Docker Engine from Docker's official repository.
  - Multi-arch (arm64) images, and what to do when an app's image has no arm64 variant.
  - Build on my Apple Silicon Mac for `linux/arm64` (native, so fast).
- **Networking at home:**
  - A static IP or DHCP reservation, and router port forwarding for 80/443.
  - A changing public IP (dynamic DNS).
  - What to do behind **CGNAT** or when ports 80/443 can't be opened (e.g. Cloudflare Tunnel, Tailscale), and how that affects Let's Encrypt (HTTP-01 vs DNS-01 challenge).
  - IPv6 considerations.
- **Security hardening:**
  - A non-default user, SSH keys only, password and root login disabled.
  - A firewall allowing only 22/80/443.
  - **The Docker gotcha: ports published by containers bypass `ufw`.** Explain how DokWalt avoids exposing anything except the reverse proxy on 80/443 (apps and databases stay on internal Docker networks).
  - Automatic security updates (unattended-upgrades) with controlled reboots.
  - fail2ban or a lighter alternative (weigh its RAM cost).
  - Disable unused services (Bluetooth, Wi-Fi when on Ethernet, avahi, …).
  - DokWalt's own attack surface on the Pi.
- **Pi health:** CPU temperature and throttling (`vcgencmd`) shown in `dokwalt doctor` and the monitoring dashboard, with alerts.
- **Backups & recovery:** what to back up (DokWalt's SQLite state, app volumes, certificates) and how to restore everything onto a fresh Pi.
- **Final checklist:** checks to run after installation, including the hard-reboot test and a real **power-cut test**, confirming the Pi is ready to serve production sites.

Also include a shorter equivalent guide for a generic Debian/Ubuntu VPS, covering only what differs from the Pi.

---

## Open source

DokWalt will be **open source**. The spec should cover:

- **License recommendation** (e.g. MIT vs Apache-2.0 vs AGPL) with the trade-offs explained.
- **Distribution:** how people install it (GitHub releases with prebuilt binaries, an install script, Homebrew tap) and how the server daemon is updated (`dokwalt server upgrade`), including version compatibility between CLI and daemon.
- **Contributor-friendliness:** a clear project layout, a README that gets someone from zero to a deployed site in minutes, docs shared between the repo and `dokwalt docs`, and a test strategy runnable locally (including the reboot test, e.g. in a VM).
- **No telemetry** by default.

---

## Performance budget (targets; validate or adjust)

- DokWalt daemon: **< 30 MB RSS idle**, **< 1% CPU idle** on a 1 vCPU VPS.
- Total platform overhead (daemon + proxy): **< 100 MB RAM**.
- Must run comfortably on a **1 GB VPS** and a **Raspberry Pi 4/5**.
- The spec must say how these numbers will be measured and enforced (e.g. a benchmark in the test suite).

---

## Non-goals (v1)

- Web UI or dashboard in the browser.
- Docker Swarm, Kubernetes, clusters, multi-node scheduling.
- Managed databases or add-ons, and automated DB backups (possibly a later version).
- GitHub integration and push-to-deploy. **Design the architecture so it can be added later** (webhook → low-priority server-side build), but don't specify it in detail.
- Review apps per pull request (later).
- CI/CD, buildpacks, paid external services.
- Multi-user accounts and permissions.
- Redis, message queues, or any extra infrastructure.

---

## Expected structure of the spec

1. **Summary**: what DokWalt is, in one paragraph.
2. **Goals & non-goals**
3. **Architecture overview**: components, diagram (ASCII or Mermaid), and how the CLI, daemon, Docker, proxy and SQLite interact.
4. **Technology choices**: each with justification and rejected alternatives (language, proxy, storage, TUI library, image transfer).
5. **Core concepts & data model**: app, stage, pipeline, release, service, config; SQLite schema.
6. **App definition**: a sample real-world `docker-compose.yml` (web + worker + Postgres), exactly what DokWalt infers from it, and the CLI commands that complete the setup on the server (domains, env vars, overrides). Also say where server-side app state is stored and how to export or import it (e.g. to recreate an app on a new server).
7. **CLI command reference**: every command with flags, examples and **mockups of terminal output**.
8. **TUI dashboard**: screens, navigation, keybindings, with mockups.
9. **Flows, step by step**: server init, first deploy, redeploy (zero-downtime), config change, rollback, add a domain, database connection, remove app, and for pipeline apps: enable pipeline, promote, disable pipeline.
10. **Reboot & self-healing**: reconciliation loop, systemd setup, failure scenarios.
11. **Certificates & routing**
12. **Logs**
13. **Metrics & alerts**: what's collected, how, storage, retention, cost.
14. **Security model**: access control, secrets, container isolation, attack surface.
15. **Resource budget**: targets and how they're measured.
16. **Failure modes & edge cases**: failed health check, disk full, DNS not ready, Let's Encrypt rate limits, interrupted transfer, power cut, and so on.
17. **Extensibility**: how GitHub push-deploy, review apps and multi-server fit in later without a redesign.
18. **Milestones**: a phased plan starting with an MVP (server init, single-environment deploy, HTTPS, logs, config, rollback, `db:connect`, survives reboot), then optional pipelines/promotion, then monitoring/alerts, then TUI polish/docs/open-source release. Each milestone has acceptance criteria.
19. **Installation guides**: the full Raspberry Pi 5 guide and the shorter VPS guide described above.
20. **Open questions**: anything you need me to decide.
