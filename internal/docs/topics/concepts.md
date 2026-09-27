# Concepts

*The handful of ideas DokWalt is built on: servers, apps, releases, config and the reconciler.*

## Architecture

```text
 Your Mac                               Server (VPS or Raspberry Pi)
┌────────────────────┐   one SSH      ┌──────────────────────────────────────┐
│ dokwalt (CLI, TUI) │  connection    │ dokwalt dial-stdio                   │
│ docker compose     │ ─────────────→ │   ↓ /run/dokwalt/dokwalt.sock        │
│   build (buildx)   │  images, API,  │ dokwalt daemon (systemd)             │
│ ~/.config/dokwalt  │  logs, tunnels │   SQLite · reconciler · metrics      │
└────────────────────┘                │   ↓ Docker Engine API                │
                                      │ ┌─────────────┐─→ dw-blog-production │
          Internet ── 80/443 ───────→ │ │dokwalt-caddy│     web, worker, db  │
                                      │ └─────────────┘─→ dw-shop-production │
                                      │                    web, redis        │
                                      └──────────────────────────────────────┘
```

- The **CLI** builds images on your Mac and talks to the daemon over SSH.
  There is no public control-plane port and no DokWalt password: access
  is your SSH key.
- The **daemon** runs natively under systemd (never in a container). It
  stores state in SQLite under `/var/lib/dokwalt` and drives Docker.
- **Caddy** (`dokwalt-caddy`) is the only container publishing ports
  (80/443). It terminates TLS and routes each domain to containers.

## Server (context)

A server context is a name for a target (`user@host`, `user@host:port`
or an ssh alias), stored on your Mac in `~/.config/dokwalt/config.json`
(override the path with `DOKWALT_CONFIG`):

```json
{"current":"pi",
 "servers":{"pi":{"target":"dd@pi.home"}},
 "links":{"/Users/dd/Code/blog":{"server":"pi","app":"blog"}}}
```

Manage them with `server init`, `server add`, `server list`, `server use`
and `server remove`. Resolution: `--server` flag, then the server of the
folder link, then the current server.

## App

An app is a Docker Compose project plus server-side settings (config,
domains, overrides). It is defined by your existing compose file; there
is no DokWalt file in the repository.

```bash
dokwalt apps:create blog     # creates the app and links this folder
dokwalt link blog            # link another folder to an existing app
```

App resolution: `-a/--app`, then the folder link (current folder or
nearest parent), otherwise `which app? pass -a <app>, or run dokwalt
link <app> in your project folder`.

## Stage and pipeline

Every app has a stage called `production`. For a normal app it is hidden:
you never type it and never see it. `dokwalt pipeline:enable` adds a
`staging` stage on the same server, with its own containers, network,
domains, config and volumes. `dokwalt promote` sends staging's exact
images to production. See `dokwalt docs pipelines`.

## Service

Each compose service is handled in one of three ways:

| Kind         | Examples                         | How it runs                          |
|--------------|----------------------------------|--------------------------------------|
| stateless    | web, worker                      | blue/green, replaced on each release |
| stateful     | db (postgres), redis, named vols | data project, never duplicated       |
| one-shot job | migrate                          | runs to completion in the new color  |

A service is **stateful** if it mounts a named volume or uses a
well-known database image (postgres, mysql, mariadb, mongo, redis,
valkey, ...). Override with `dokwalt services:set <svc> --stateful=true|false`.

A service is a **one-shot job** when another service depends on it with
`condition: service_completed_successfully`. It runs on every release,
before its dependents start, and must exit 0: this is the release phase
for database migrations. See `dokwalt docs compose`.

A service is **public** when a domain points to it. Public or not, every
service sits on the stage's private network; Caddy is attached to that
network and reaches containers by name.

## Networks and isolation

Each stage has its own bridge network, `dw-<app>-<stage>`. Services join
it with their service name as alias, so `db:5432` works as it does
locally. There is no shared proxy network: the daemon attaches
`dokwalt-caddy` to every stage network. Apps cannot reach each other,
and neither can staging and production of the same app.

## Release

A release is an immutable, numbered snapshot per stage: the images
(content-addressed tags), the normalized compose model exactly as the
CLI sent it, a config version, a description (`-m`), the git sha and a
status. The model is transformed at deploy time for the target stage, so
a rollback or promotion is re-transformed with today's stage settings.
Deploys, config changes, rollbacks and promotions each create one.

```text
$ dokwalt releases
RELEASE   STATUS         DESCRIPTION                          COMMIT    CONFIG   CREATED
▶ v8      ● live         new homepage                         3f2c1ab   c5       2m ago
  v7      ● failed       service web is crash-looping         d41e0b2   c5       1h ago
  v6      ● superseded   Set LOG_LEVEL                        9a01d2e   c5       1d ago
  v5      ● superseded   deploy                               9a01d2e   c4       3d ago
```

Statuses: `deploying`, `succeeded`, `failed`, `superseded` (the current
release shows as `live`). Images of the last 5 successful releases per
stage are kept, so `dokwalt rollback` needs no build or upload.

## Config and config versions

Config is a set of `KEY=VALUE` pairs, encrypted at rest on the server.
Every change creates a new **config version** (`c<N>`) and, once the
stage is deployed, a new release. Values never touch disk in clear text:
stateless services get each key as a pass-through entry (`KEY: null`) and
compose reads the values from the environment of the `docker compose`
process the daemon runs. See `dokwalt docs config`.

## Colors: blue and green

Stateless services and one-shot jobs run in one of two compose projects,
`dw-blog-production-blue` or `dw-blog-production-green`. Each release
starts the other color, waits until it is ready and healthy, switches
Caddy to it, drains the old one for 10 s and stops it. Only one color
runs outside a release.

## Data project

Stateful services live in their own project, `dw-blog-production-data`.
It is never duplicated: each release runs `up -d` on it, which only
recreates a container when its definition changed. Named volumes are
created by the daemon as `dw-blog-production-pgdata`, shared by the
data, blue and green projects and never removed by a deploy.

## Names at a glance

| Thing             | Name                                                  |
|-------------------|-------------------------------------------------------|
| Data project      | `dw-blog-production-data`                             |
| Stateless project | `dw-blog-production-blue` / `-green`                  |
| Container         | `dw-blog-production-green-web-1`                      |
| Stage network     | `dw-blog-production` (alias = service name)           |
| Named volume      | `dw-blog-production-pgdata`                           |
| Built image       | `dokwalt/blog-web:3f9c2a1b7d0e` (first 12 hex of ID)  |
| Rendered files    | `/var/lib/dokwalt/apps/blog/production/<project>.json` |
| Caddy             | project `dokwalt-system`, container `dokwalt-caddy`   |

## Domains

Domains are explicit, one per DNS record, per app and stage. No wildcards.
Caddy obtains and renews Let's Encrypt certificates automatically (local
names such as `.lan` or `.test` get a certificate from Caddy's internal
CA). Redirects (`www` → apex) are domains too. See `dokwalt docs domains`.

## One operation at a time

Mutating operations (deploy, rollback, promote, config change, restart,
stop/start, ...) take one server-wide lock. A second one waits for the
first to finish, then runs; it never fails with "in progress".

## Reconciler

The daemon keeps actual state equal to desired state (SQLite). It runs:

- at startup (after waiting for Docker, marking interrupted `deploying`
  releases failed and tearing down their color),
- 3 s after a Docker die, OOM or destroy event on a DokWalt container,
- every 60 seconds (a pass is skipped while an operation holds the lock).

It ensures the Caddy project, each stage network with Caddy attached, and
every deployed stage's data project and active color, running
`docker compose up -d` only when a service has no running container. It
removes leftover inactive colors and re-applies the Caddy config if it
drifted. One-shot jobs are never restarted. This is why sites come back
after `sudo reboot` or a power cut. A stage stopped with `dokwalt stop`
stays stopped until `dokwalt start`.

See also: `dokwalt docs getting-started`, `dokwalt docs deploy`,
`dokwalt docs compose`, `dokwalt docs config`, `dokwalt docs pipelines`,
`dokwalt docs reboot-recovery`.
