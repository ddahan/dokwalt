# Docker Compose apps

*How DokWalt reads your existing compose file and turns it into safe, zero-downtime projects.*

## Your compose file is the app definition

DokWalt uses the compose file already in your repository (`compose.yaml`
or `docker-compose.yml`, or the files given with `deploy -f`). There is
no DokWalt file and nothing to rewrite. Everything that varies between
deploys (config, domains, health checks, overrides) lives on the server.

On each deploy the CLI runs, in your project folder:

```bash
docker compose config --no-interpolate --format json
```

Compose normalizes `extends`, profiles, short syntax and relative paths,
but `--no-interpolate` keeps `${VAR}` references as they are: your computer's
environment and `.env` never leak into production. The CLI sends this
model unchanged; the release stores it as-is and the daemon transforms
it for the target stage at deploy time. Your file is never modified.

## A typical app

```yaml
services:
  migrate:
    build: .
    command: ["./bin/migrate", "up"]
    depends_on:
      db: { condition: service_healthy }
  web:
    build: .
    command: ["./bin/server"]
    environment:
      PORT: "3000"
      DATABASE_URL: postgres://blog:${DB_PASSWORD}@db:5432/blog
    ports: ["3000:3000"]            # removed on the server, port remembered
    depends_on:
      migrate: { condition: service_completed_successfully }
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:3000/healthz"]
      interval: 5s
      retries: 10
  worker:
    build: .
    command: ["./bin/worker"]
  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: blog
      POSTGRES_DB: blog
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U blog"]
      interval: 5s
volumes:
  pgdata:
```

Complete it on the server:

```bash
dokwalt config:set DB_PASSWORD=s3cret
dokwalt deploy
dokwalt domains:add blog.example.com          # web is the only service with a port
```

`migrate`, `web` and `worker` build from the same Dockerfile, so they
share one image, uploaded once. Services reach each other by service
name (`db:5432`) on the stage network.

```text
$ dokwalt services
SERVICE   ON EACH DEPLOY                                                                WHY                                     HEALTH CHECK
db        keeps its data (never duplicated; restarted only if its definition changes)   named volume pgdata                     default
migrate   zero-downtime swap (a new copy starts, then replaces the old one)             no named volume, not a database image   default
web       zero-downtime swap (a new copy starts, then replaces the old one)             no named volume, not a database image   default
worker    zero-downtime swap (a new copy starts, then replaces the old one)             no named volume, not a database image   default
Override: dokwalt services:set <service> --stateful=false
```

## One-shot jobs: migrations and release tasks

A service that another service depends on with
`condition: service_completed_successfully` is a **one-shot job**
(`migrate` above). DokWalt sets `restart: "no"` and the label
`dokwalt.oneshot=true`, and:

- runs it in the **new color** on every release (deploy, config change,
  rollback, promote), after the data project is ready and before its
  dependents start;
- requires exit code 0: otherwise the deploy fails and the old version
  keeps serving;
- never restarts it (reconciler, `dokwalt restart`) and excludes it from
  crash-loop alerts; `dokwalt exec` never picks it by default.

Old code keeps serving until the switch, so migrations must be backward
compatible (expand/contract). `migrate → db` crosses the data/color split
and is dropped: the data project is always started and ready first.

## Transformation rules

| Compose key                          | What DokWalt does                                          |
|--------------------------------------|------------------------------------------------------------|
| `build:`                             | Removed; `image` = uploaded `dokwalt/<app>-<svc>:<id12>`   |
| `image:` without `build:`            | Kept, `pull_policy: missing`; not pinned by digest         |
| `ports:`                             | Removed ⚠; container ports kept as defaults for `domains:add`, `db:connect` |
| `expose:`                            | Kept; also counts as declared ports                        |
| `container_name:`                    | Removed ⚠ (would collide between blue and green)           |
| `env_file:`                          | Removed ⚠; use `dokwalt config:import .env`                |
| `links:`, `external_links:`          | Removed ⚠; services reach each other by name               |
| `network_mode:` other than `bridge`  | Rejected ✗                                                 |
| service-level `secrets:`, `configs:` | Rejected ✗; use `dokwalt config:set`                       |
| bind mount inside the project folder | Rejected ✗ (`./initdb:/docker-entrypoint-initdb.d`)        |
| other absolute host bind mount       | Allowed ⚠ "host path must exist on the server"             |
| named volume                         | External volume `dw-<app>-<stage>-<vol>`, created by the daemon |
| `external: true` volume              | Kept as-is                                                 |
| `networks:`                          | Replaced by the stage network, service name as alias       |
| top-level `x-dokwalt: {uses: [app]}` | Those apps' stateful services join the stage network, by app name |
| `depends_on` across data/color split | Dropped, both directions                                   |
| `restart:`                           | Default `unless-stopped` (`"no"` for one-shot jobs)        |
| `logging:`                           | Default `local`, 10m × 3 if unset                          |
| `labels:`                            | Kept; `dokwalt.*` labels set (overwritten) by DokWalt      |
| `environment:`                       | Kept; stateless services also get each config key as `KEY: null` |
| `healthcheck:`                       | Kept, gates the switch (`disable: true` counts as none)    |
| everything else                      | Kept verbatim (`command`, `user`, `mem_limit`, ...)        |

Warnings (⚠) are shown during deploy and do not stop it. Rejections (✗)
stop the deploy before a release is created; nothing changes:

```text
✗ service web: bind mount of a project file (uploads) is not possible on the server — bake it into the image or use a named volume
```

A `${VAR}` without default that is not in config warns
`${X} is used in the compose file but not set` and becomes empty.

Why remove `ports:`? Ports published by Docker bypass `ufw`. With DokWalt
nothing but Caddy is reachable from outside; databases are never exposed.

## Stateful or stateless

Detection, in order: a `services:set` override; a named volume; a
well-known image (base name or `<name>-*` variant): postgres, postgis,
timescaledb, mysql, mariadb, percona, mongo, redis, valkey, keydb,
dragonfly, memcached, rabbitmq, elasticsearch, opensearch, clickhouse,
influxdb, minio, cassandra, couchdb, neo4j, nats, meilisearch, typesense,
qdrant. Absolute bind mounts do not make a service stateful.

| Stateful services                    | Stateless services                  |
|--------------------------------------|-------------------------------------|
| run in `dw-blog-production-data`     | run in `-blue` / `-green`           |
| never duplicated                     | new color on every release          |
| recreated only if definition changed | replaced on every release           |
| see only `${VAR}` config             | every config key passed through     |

Fix a wrong guess with an override stored on the server. It redeploys the
current release immediately:

```bash
dokwalt services:set cache --stateful=false   # throwaway cache: blue/green
dokwalt services:set web --stateful=true      # SQLite app that must never run twice
```

## Names on the server

| Thing                  | Name                                                 |
|------------------------|------------------------------------------------------|
| Data project           | `dw-blog-production-data`                            |
| Stateless projects     | `dw-blog-production-blue`, `-green`                  |
| Stage network          | `dw-blog-production` (Caddy attached, no other app)  |
| Volume `pgdata`        | `dw-blog-production-pgdata`                          |
| Built image            | `dokwalt/blog-web:3f9c2a1b7d0e`                      |
| Rendered compose files | `/var/lib/dokwalt/apps/blog/production/dw-blog-production-{data,blue,green}.json` |

Labels: `dokwalt.app`, `dokwalt.stage`, `dokwalt.service`, `dokwalt.role`
(`data`/`app`), `dokwalt.color` and `dokwalt.release` (stateless only),
`dokwalt.oneshot=true` (jobs).

## Config: interpolation and pass-through

- **Interpolation**: `${VAR}` anywhere in the compose file is resolved on
  the server. Works for every service, stateful included.
- **Pass-through**: every config key is added to each **stateless**
  service as `KEY: null`; a config key wins over an `environment:` entry
  of the same name.

Both read the environment of the `docker compose` process the daemon
runs, so rendered files never contain values. Stateful services only see
what you interpolate: `db` above uses `POSTGRES_PASSWORD: ${DB_PASSWORD}`.
A config change recreates the database only if it changes a variable its
definition uses. See `dokwalt docs config`.

## Shared services: `x-dokwalt`

The only DokWalt-specific key, at the top level of the compose file. It lets
an app reach another app's stateful services, typically one Postgres for
every app on the server:

```yaml
x-dokwalt:
  uses: [postgres]   # then connect to postgres:5432
```

Compose ignores `x-` keys, so the file still works with
`docker compose up` locally. Details: `dokwalt docs databases`.

## Checklist: things to fix in your compose

- **Project bind mounts** (`./data:/data`): use a named volume, or `COPY`
  the files into the image.
- **`ports:`**: harmless, removed. Expose the app with `domains:add`.
- **`env_file:`**: run `dokwalt config:import .env` once instead.
- **Migrations in `command:`**: move them to a one-shot job.
- **Local-only services** (mailhog, adminer): keep them in a separate
  compose file or profile you do not deploy.
- **Listen on 0.0.0.0**, not `localhost`, inside the container, or Caddy
  and health checks cannot reach it.
- **arm64 images** for an arm64 server: check with
  `docker manifest inspect postgres:17 | grep arm64`, or add `build:`.

See also: `dokwalt docs deploy`, `dokwalt docs config`,
`dokwalt docs concepts`, `dokwalt docs domains`, `dokwalt docs databases`,
`dokwalt docs server`.
