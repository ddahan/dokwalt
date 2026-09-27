# Getting started

*From an empty server to a site served over HTTPS in about ten minutes.*

## What you need

On your Mac:

- **Docker Desktop (Docker 28+) or OrbStack** with the Compose v2 plugin.
  DokWalt builds your images locally, so `docker compose version` must
  work. Docker 28+ is needed for `docker save --platform`.
- An **SSH key** that can log in to the server (`ssh dd@pi.home` works).
  Agent keys are tried first, then identity files. `~/.ssh/config`
  aliases and `ProxyJump` are honoured.
- A project folder with a `Dockerfile` and a compose file
  (`compose.yaml` or `docker-compose.yml`).

On the server:

- A 64-bit Linux with systemd (Debian, Ubuntu, Raspberry Pi OS), amd64
  or arm64. 32-bit Raspberry Pi OS is refused.
- A user with `sudo` (or root). You type the sudo password yourself.
- Ports **80 and 443** reachable from the internet (router port forwarding
  at home, firewall rules on a VPS).
- A **DNS A/AAAA record** for each domain, pointing to the server.

Docker does not need to be installed on the server: `server init` does it.

## 1. Install the CLI

Build it from source (needs Go):

```bash
git clone https://github.com/ddahan/dokwalt && cd dokwalt
make build
sudo cp bin/dokwalt* /usr/local/bin/
dokwalt version
```

`make build` produces the Mac CLI plus `dokwalt_linux_amd64` and
`dokwalt_linux_arm64`. Keep them next to the CLI: `server init` uploads
the one matching the server. Alternatively, download the `darwin` binary
from https://github.com/ddahan/dokwalt/releases and put it in your
`PATH` as `dokwalt`; a release build downloads the server binary for its
own version and checks it against `checksums.txt`. An install script and
a Homebrew tap are planned, not shipped.

## 2. Set up the server

```bash
dokwalt server init dd@pi.home --name pi --email you@example.com
```

The target is `user@host`, `user@host:port` or any `~/.ssh/config`
alias. `--email` is the Let's Encrypt account email. `--name` sets the
context name (default: the host's first label, `default` for an IP). The
first server you add becomes the current one.

```text
◆ Installing DokWalt on dd@pi.home
✓ Connecting over SSH
✓ Uploading dokwalt for linux/arm64 (14.2 MiB)
◆ Running the installer with sudo on the server (you may be asked for your password)
[sudo] password for dd:
◆ Installing DokWalt  arm64
→ Installing Docker (official get.docker.com script)
✓ Docker 28.4.0
✓ Docker Compose 2.39.2
→ Configuring Docker: live-restore (containers survive Docker restarts), bounded logs
✓ Docker configured
✓ User dd can manage DokWalt (groups dokwalt, docker)
✓ Installed /usr/local/bin/dokwalt
→ Waiting for the daemon
✓ Daemon running (systemd unit dokwalt.service, starts at boot)
✓ Let's Encrypt account email: you@example.com

✓ dd@pi.home is ready — saved as server pi
```

What init did: upload the Linux binary, install Docker if missing (and
the compose plugin), merge `live-restore: true` and `log-driver: local`
into `/etc/docker/daemon.json`, create the `dokwalt` group and add you to
`dokwalt` and `docker`, install `/usr/local/bin/dokwalt`, create
`/var/lib/dokwalt`, install and start the systemd unit. The daemon then
starts Caddy (`dokwalt-caddy`) and keeps it running.

Nothing listens on a new public port except Caddy on 80/443. The CLI
always talks to the daemon through SSH.

## 3. Create the app

```bash
cd ~/Code/blog
dokwalt apps:create blog
```

```text
✓ Created blog on pi
  Linked to /Users/dd/Code/blog
  Next: dokwalt deploy then dokwalt domains:add example.com --service web
```

`apps:create` links the current folder by default (`--no-link` to skip;
`dokwalt link blog` links another folder later). The link lives in
`~/.config/dokwalt/config.json` on your Mac; nothing is written to the
repository. From this folder (or any subfolder) you can omit `-a blog`.

## 4. Set config

Config lives on the server, encrypted. Import your `.env` and add values:

```bash
dokwalt config:import .env
dokwalt config:set DB_PASSWORD=$(openssl rand -hex 16) NODE_ENV=production
dokwalt config
```

```text
◆ Config of blog
KEY           VALUE
DB_PASSWORD   9f••••••••••••
NODE_ENV      pr••••••••
SMTP_HOST     sm••••••••••••
```

Before the first deploy this only stores values ("applies on first
deploy"). Stateless services receive every key as an environment
variable; `${VAR}` in the compose file is interpolated on the server.
See `dokwalt docs config`.

## 5. Deploy

```bash
dokwalt deploy -m "first deploy"
```

```text
◆ Deploying blog  to pi (linux/arm64)
✓ Built migrate, web, worker
✓ Uploaded 41.3 MiB in 7.2s
! web: published ports removed — traffic reaches services only through the proxy (use `dokwalt domains:add` or `dokwalt db:connect`)
✓ Services — db: stateful (named volume pgdata), migrate: one-shot job, web: stateless, worker: stateless
✓ Created release v1
✓ Stateful services ready
✓ Containers running
✓ v1 is live

✓ blog is live — no domain yet
  Add one: dokwalt domains:add example.com --service <service>
```

The first deploy has no domain yet, so the app runs but is not public.
Registry images such as `postgres:17` are pulled on the server if absent.

## 6. Add a domain

Create the DNS record first, then:

```bash
dokwalt domains:add blog.example.com --service web --port 3000
```

```text
  blog.example.com → web:3000
✓ blog.example.com → reachable; HTTPS certificate is issued automatically
```

Both flags are optional: `--service` defaults to the only stateless
service that declares a port, `--port` to its first declared port. The
domain is always added, without a prompt. If DNS or port forwarding is
not ready, the CLI prints `... was added but isn't reachable yet:` with
the reason; Caddy keeps retrying the certificate. Redirect `www` with
`dokwalt domains:redirect www.blog.example.com blog.example.com`.

## 7. Check it

```bash
dokwalt ps
dokwalt logs -f
dokwalt doctor
```

```text
$ dokwalt ps
SERVICE   CONTAINER                            STATE        HEALTH    RESTARTS   STATUS
db        dw-blog-production-data-db-1         ● running    healthy   0          Up 3 minutes (healthy)
migrate   dw-blog-production-blue-migrate-1    ✓ completed  —         0          Exited (0) 2 minutes ago
web       dw-blog-production-blue-web-1        ● running    healthy   0          Up 2 minutes (healthy)
worker    dw-blog-production-blue-worker-1     ● running    —         0          Up 2 minutes
```

`doctor` checks the connection and versions, Docker, live-restore, the
memory cgroup, the compose plugin, start on boot, disk, memory,
temperature, clock, the daemon, Caddy, the Let's Encrypt email and each
domain.

## Everyday loop

```bash
git commit -am "new homepage"
dokwalt deploy                     # zero-downtime; -m defaults to the commit subject
dokwalt releases                   # history
dokwalt rollback                   # back to the previous release, no prompt
```

Run `dokwalt` with no arguments for the full-screen dashboard.

## Next steps

- Reboot the server (`sudo reboot`) and watch the site come back by itself.
- Add alerts: `dokwalt alerts:add discord <webhook-url>`.
- Open a database shell: `dokwalt db:connect`.

See also: `dokwalt docs concepts`, `dokwalt docs deploy`,
`dokwalt docs compose`, `dokwalt docs domains`, `dokwalt docs raspberry-pi`,
`dokwalt docs vps`.
