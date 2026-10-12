# Databases

*Databases are ordinary compose services: kept in one stable place, never exposed, opened with their own client inside the container.*

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
SERVICE   ON EACH DEPLOY                                                                WHY                                     HEALTH CHECK
db        keeps its data (never duplicated; restarted only if its definition changes)   named volume pgdata                     default
web       zero-downtime swap (a new copy starts, then replaces the old one)             no named volume, not a database image   default
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

## Opening a database shell

```bash
dokwalt db:connect
```

1. **Service**: `--service`, or else the first service with a recognised
   database image. SQL databases are preferred over Redis-like ones. The
   stage must be deployed and the service running.
2. **Client**: the database's own client runs **inside its container** on
   the server (`docker exec` over SSH), logged in with the credentials read
   from the container's environment. Nothing to install on your computer,
   and nothing is exposed. Exit the client to come back.

```text
$ dokwalt db:connect
◆ postgres on db (blog) — exit the shell to come back
psql (17.2)
blog=# \dt
```

Pipe a script to run it without a prompt:

```bash
echo "SELECT count(*) FROM posts;" | dokwalt db:connect
dokwalt db:connect < fix.sql
```

### Other services, databases and stages

```bash
dokwalt db:connect --service cache               # the redis service
dokwalt db:connect -s staging                    # staging's database
dokwalt db:connect -a postgres --database blog   # another database of the server
```

For an image DokWalt doesn't recognise, `db:connect` stops and points to
`dokwalt exec --service <svc> -- <client>`.

## Supported kinds

| Kind     | Images                               | Client (in the container) | Credentials from the container env      |
|----------|--------------------------------------|-------------|--------------------------------------------------------|
| postgres | postgres, postgis, timescaledb       | `psql`      | `POSTGRES_USER` (default `postgres`), `POSTGRES_PASSWORD`, `POSTGRES_DB` (default: the user) |
| mysql    | mysql, mariadb, percona              | `mysql`, else `mariadb` | `MYSQL_USER`/`MARIADB_USER` + `*_PASSWORD`, else `root` + `MYSQL_ROOT_PASSWORD`/`MARIADB_ROOT_PASSWORD`; `MYSQL_DATABASE`/`MARIADB_DATABASE` |
| mongo    | mongo                                | `mongosh`, else `mongo` | `MONGO_INITDB_ROOT_USERNAME`/`PASSWORD`, `MONGO_INITDB_DATABASE` |
| redis    | redis, valkey, keydb, dragonfly      | `redis-cli`, else `valkey-cli`, `keydb-cli` | `REDIS_PASSWORD` or `VALKEY_PASSWORD`, if set          |

Passwords reach the client through its environment (`PGPASSWORD`,
`MYSQL_PWD`, `REDISCLI_AUTH`; for `mongosh`, a `mongodb://` URL with
`authSource=admin`), set with `docker exec -e`: they are never arguments of
`docker exec`. Postgres connects through the container's Unix socket, where the
official images trust local users. An image without its kind's client (some
Dragonfly images have no `redis-cli`) fails with `no redis-cli in the cache
container`.

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

**Nightly off-site backups** of every Postgres database, to Cloudflare R2 or
any S3 storage, run on the server: `dokwalt backup:setup`, see
`dokwalt docs backups`. For other kinds of databases, and for one-off copies,
two approaches.

**Logical dump** to your computer, while the app runs:

```text
$ dokwalt db:backup
ℹ Dumping database blog from db (production)…
✓ Saved blog-production-20261010-153000.dump (2.4 MB in 1.2s)
  Restore: pg_restore --clean --if-exists --no-owner -d <postgres-url> blog-production-20261010-153000.dump
```

- `pg_dump --format=custom` runs **inside** the database container, through
  its Unix socket, and the dump streams back over SSH. Nothing is exposed, and
  no Postgres client is needed on your computer.
- The file is written as `<file>.part` and renamed only once pg_dump
  succeeded and the file is a valid archive, so a failed dump never leaves a
  file that looks good.
- `-o <file>` picks the file, `--service` the database service, and
  `--database` another database of the same server.
- Postgres only. For other kinds, run the kind's own dump tool with
  `dokwalt exec --service <db> -- …`.

`dokwalt exec` allocates a terminal, so never redirect binary output (a dump)
through it.

**Volume archive** on the server. Stop the stage first for a consistent copy:

```bash
dokwalt stop
ssh prod 'docker run --rm -v dw-blog-production-pgdata:/data:ro \
  -v "$HOME":/backup alpine tar czf /backup/pgdata.tgz -C /data .'
dokwalt start
```

Restore a dump with `pg_restore -d "postgres://…@127.0.0.1:5433/blog"`.
Full-server backups: `dokwalt docs server`.

## One database server for several apps

Running one Postgres per app costs memory on a small server. Instead, deploy
the database server as its own app, and let other apps **use** it:

```yaml
# postgres/compose.yaml: an ordinary app with one stateful service
services:
  db:
    image: postgres:18-alpine
    environment:
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes:
      - pgdata:/var/lib/postgresql
volumes:
  pgdata:
```

```yaml
# blog/compose.yaml
x-dokwalt:
  uses: [postgres]          # app names
services:
  web:
    build: .
    environment:
      DATABASE_URL: postgres://blog:${BLOG_DB_PASSWORD}@postgres:5432/blog
```

- **Networking.** The provider's stateful containers join the consumer's
  private network under the provider's **app name** (`postgres`), and also as
  `<app>-<service>` (`postgres-db`). When the provider has several stateful
  services, only the `<app>-<service>` names exist. Consumers still share no
  network with each other: `blog` can't reach another app that uses
  `postgres`.
- **Stages.** A consumer stage uses the provider stage with the same name
  if it exists, otherwise its production.
- **Order.** Deploy the provider first. A consumer deploy fails early if the
  provider is missing or not deployed, and attaches the provider **before**
  the new version starts, so migrations can already reach it.
- **Self-healing.** When the provider's container is recreated (an image
  change, a reboot), the reconciler attaches it again within seconds. Removing
  `uses` and redeploying detaches it.
- **Safety.** `apps:destroy` and `apps:rename` refuse a provider that other
  apps still use.
- **One role and database per app.** DokWalt doesn't create them; do it once
  per app with `dokwalt db:connect -a postgres`:
  `CREATE ROLE blog LOGIN PASSWORD '…'; CREATE DATABASE blog OWNER blog;`
- **From a consumer**, `db:connect` and `db:backup` find the shared database
  and use the app's own role and database, read from the connection URL whose
  host is the provider (config vars first, `DATABASE_URL` preferred, then the
  compose environment). Without such a URL, they use the provider's superuser
  on a database named after the app.

The provider stays an ordinary app: it has its own config, releases and
logs, and nothing about it is managed. Backups matter more now that one
server holds every app's data: nightly backups (`dokwalt docs backups`) dump
each of its databases separately, with its roles.

## Major version upgrades

Changing `postgres:16` to `postgres:17` reuses the same volume, and
PostgreSQL refuses to start on files from another major version. The data
project is updated first, so this means **downtime and a failed deploy**.
Instead:

1. Dump the database (above).
2. In compose, change the image **and** the volume name (`pgdata17`).
3. `dokwalt deploy` creates a fresh, empty `dw-blog-production-pgdata17`.
4. Restore the dump into it, piped through `exec` (piped input gets no
   terminal, so the file arrives intact):
   `dokwalt exec --service db -- pg_restore --no-owner -U postgres -d blog < blog.dump`.
5. Once verified, delete the old volume on the server:
   `ssh prod docker volume rm dw-blog-production-pgdata`.

Minor updates are safe: change the tag (`postgres:17.1` → `postgres:17.2`)
and redeploy. Registry images are used by tag, not pinned by digest, and
are pulled only if the tag is missing on the server. So a floating tag like
`postgres:17` doesn't move by itself. Pin exact tags.

See also: `dokwalt docs backups`, `dokwalt docs compose`, `dokwalt docs config`, `dokwalt docs deploy`,
`dokwalt docs server`, `dokwalt docs security`
