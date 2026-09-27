# Databases

*Databases are ordinary compose services: kept in one stable place, never exposed, reachable through an SSH tunnel.*

DokWalt has no managed add-ons. A database is a service in your compose
file, and DokWalt handles it correctly because it detects it as
**stateful**.

## How DokWalt treats a database

```yaml
services:
  web:
    build: .
    environment:
      DATABASE_URL: postgres://blog:${POSTGRES_PASSWORD}@db:5432/blog
  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: blog
      POSTGRES_DB: blog
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
```

- **Stateful detection**, in this order: a `services:set` override, then a
  named volume, then a well-known image (postgres, postgis, timescaledb,
  mysql, mariadb, percona, mongo, redis, valkey, keydb, dragonfly,
  memcached, rabbitmq, elasticsearch, opensearch, clickhouse, influxdb,
  minio, cassandra, couchdb, neo4j, nats, meilisearch, typesense, qdrant,
  and `<name>-*` variants). Absolute host bind mounts don't make a service
  stateful.
- **Data project**: stateful services run in the compose project
  `dw-blog-production-data`. It is never duplicated during blue/green
  deploys, and `up -d` only recreates a container if its definition changed.
- **Stable volumes**: named volumes become external volumes
  `dw-<app>-<stage>-<vol>`, here `dw-blog-production-pgdata`. With a
  pipeline, staging has its own: `dw-blog-staging-pgdata`.
- **Never public**: `ports:` are removed (with a warning). The database is
  only reachable on the stage's private network `dw-blog-production`, under
  its service name, so the app connects to `db:5432`. Other apps can't
  reach it at all.
- **Deploys never delete volumes**: old colors are removed with
  `docker compose down`, which leaves volumes alone. Only `apps:destroy`
  and `pipeline:disable` delete volumes, so back them up first.

Check or fix detection:

```text
$ dokwalt services
SERVICE  DEPLOY MODE                                     WHY                                    HEALTH CHECK
db       stateful (updated in place, never duplicated)   named volume pgdata                    default
web      blue/green (zero downtime)                      no named volume, not a database image  default
  Override: dokwalt services:set <service> --stateful=false

$ dokwalt services:set cache --stateful=false   # e.g. a throwaway redis
```

`services:set` stores the override and redeploys the current release right
away. Moving a service between the data and color projects recreates its
container, and named volumes are kept.

## Config and restarts

Config values are never written to disk. Stateless services get every
config key as an environment variable. All services, stateful included, can
use `${VAR}` interpolation. Stateful services only see the values they
reference as `${VAR}`. So:

- `dokwalt config:set SMTP_HOST=…` creates a release that restarts `web`
  with zero downtime and never touches `db`.
- Changing a value the db definition references (`POSTGRES_PASSWORD` above)
  changes that definition, so `db` is recreated at the next release. That
  means a short restart, not zero downtime.
- Most images only read `POSTGRES_PASSWORD` and similar variables on first
  init. Changing it later does **not** change the real password: run
  `ALTER USER` first, then update the config.

## Connecting from your Mac

```bash
dokwalt db:connect
```

1. **Service**: `--service`, or else the first service with a recognised
   database image. SQL databases are preferred over Redis-like ones. The
   stage must be deployed and the service running.
2. **Port**: `--remote-port`, or else the kind's default port (5432, 3306,
   27017, 6379), or else the service's first declared port.
3. **Tunnel**: a local port (`--port`, default a random free port) on
   `127.0.0.1`, tunnelled through SSH to the container. Nothing is exposed
   on the server.
4. **Client**: the matching local client starts with credentials read from
   the container's environment. The tunnel closes when you exit it.

```text
$ dokwalt db:connect
◆ Connected to db (postgres) through SSH — exit the shell to close the tunnel
psql (17.2)
blog=# \dt
```

### GUI tools (TablePlus, DBeaver, Postico…)

```text
$ dokwalt db:connect --tunnel-only --port 5433
◆ Tunnel to db (postgres)
Local address   127.0.0.1:5433
URL             postgres://blog:s3cr3t@127.0.0.1:5433/blog
  Press Ctrl+C to close the tunnel
```

Use `--port` for a fixed local port so a saved GUI connection keeps working.
The URL contains the password in clear text. If the local client isn't
installed, DokWalt says `psql not found on this machine — keeping the
tunnel open instead` and behaves like `--tunnel-only`.

### Other services and stages

```bash
dokwalt db:connect --service cache               # the redis service
dokwalt db:connect -s staging                    # staging's database
dokwalt db:connect --service search --remote-port 7700 --tunnel-only
```

For an image DokWalt doesn't recognise, you get a plain `tcp://` tunnel
(always tunnel-only). If no port is known, it fails with
`unknown port for <svc> — pass --remote-port`.

## Supported kinds

| Kind     | Images                               | Client      | Credentials from the container env                    |
|----------|--------------------------------------|-------------|--------------------------------------------------------|
| postgres | postgres, postgis, timescaledb       | `psql`      | `POSTGRES_USER` (default `postgres`), `POSTGRES_PASSWORD`, `POSTGRES_DB` (default: the user) |
| mysql    | mysql, mariadb, percona              | `mysql`     | `MYSQL_USER`/`MARIADB_USER` + `*_PASSWORD`, else `root` + `MYSQL_ROOT_PASSWORD`/`MARIADB_ROOT_PASSWORD`; `MYSQL_DATABASE`/`MARIADB_DATABASE` |
| mongo    | mongo                                | `mongosh`   | `MONGO_INITDB_ROOT_USERNAME`/`PASSWORD`, `MONGO_INITDB_DATABASE` |
| redis    | redis, valkey, keydb, dragonfly      | `redis-cli` | `REDIS_PASSWORD` or `VALKEY_PASSWORD`, if set          |

Passwords reach `psql`, `mysql` and `redis-cli` through the environment
(`PGPASSWORD`, `MYSQL_PWD`, `REDISCLI_AUTH`), not command-line arguments.
`mongosh` is the exception: it gets a `mongodb://` URL that includes the
password (with `authSource=admin`).

Install clients with Homebrew: `brew install libpq` (psql),
`brew install mysql-client`, `brew install mongosh`, `brew install redis`.

## Migrations: one-shot jobs

During a deploy, the old version keeps serving until traffic switches.
DokWalt's release phase is a **one-shot job**: a service that another
service depends on with `condition: service_completed_successfully`.

```yaml
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
    depends_on:
      migrate: { condition: service_completed_successfully }
  db:
    image: postgres:17-alpine
    # …
```

- It runs in the **new** color before its dependents start, on **every**
  release: deploys, config changes, rollbacks and promotes.
- A non-zero exit fails the deploy. The old version keeps serving, and the
  job's last log lines are shown.
- It gets `restart: "no"`. The reconciler never restarts it, `restart`
  skips it, and it never triggers crash-loop alerts.
- `depends_on: db` crosses the data/color split, so DokWalt drops it. That
  is harmless: the data project is always started and ready first.
- The same file works locally with `docker compose up`.

Keep migrations **backward compatible** (expand, then contract): the old
code runs against the migrated schema until the switch, and again after a
rollback. A `rollback` doesn't undo migrations or data changes. For a one-off
command: `dokwalt exec --service web -- ./bin/migrate status`.

## Backups

DokWalt doesn't back up your databases automatically. Two approaches.

**Logical dump (recommended)** through the tunnel, while the app runs:

```bash
dokwalt db:connect --tunnel-only --port 5433     # terminal 1
pg_dump -Fc -f blog-$(date +%F).dump \
  "postgres://blog:s3cr3t@127.0.0.1:5433/blog"   # terminal 2
```

Or dump inside the container, then copy the file off the server. `exec`
allocates a terminal, so never redirect binary output through it:

```bash
dokwalt exec --service db -- pg_dump -U blog -Fc -f /tmp/blog.dump blog
ssh pi.home docker cp dw-blog-production-data-db-1:/tmp/blog.dump .
scp pi.home:blog.dump .
```

**Volume archive** on the server. Stop the stage first for a consistent copy:

```bash
dokwalt stop
ssh pi.home 'docker run --rm -v dw-blog-production-pgdata:/data:ro \
  -v "$HOME":/backup alpine tar czf /backup/pgdata.tgz -C /data .'
dokwalt start
```

Restore a dump with `pg_restore -d "postgres://…@127.0.0.1:5433/blog"`.
Full-server backups: `dokwalt docs raspberry-pi`.

## Major version upgrades

Changing `postgres:16` to `postgres:17` reuses the same volume, and
PostgreSQL refuses to start on files from another major version. The data
project is updated first, so this means **downtime and a failed deploy**.
Instead:

1. Dump the database (above).
2. In compose, change the image **and** the volume name (`pgdata17`).
3. `dokwalt deploy` creates a fresh, empty `dw-blog-production-pgdata17`.
4. Restore the dump through `db:connect --tunnel-only`.
5. Once verified, delete the old volume on the server:
   `ssh pi.home docker volume rm dw-blog-production-pgdata`.

Minor updates are safe: change the tag (`postgres:17.1` → `postgres:17.2`)
and redeploy. Registry images are used by tag, not pinned by digest, and
are pulled only if the tag is missing on the server. So a floating tag like
`postgres:17` doesn't move by itself. Pin exact tags.

See also: `dokwalt docs compose`, `dokwalt docs config`, `dokwalt docs deploy`,
`dokwalt docs raspberry-pi`, `dokwalt docs security`
