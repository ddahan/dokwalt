# Deploying

*How `dokwalt deploy` builds on your Mac, ships only changed images and swaps versions with zero downtime.*

## Usage

```bash
dokwalt deploy                          # from a linked folder
dokwalt deploy -m "new homepage"        # description (default: last commit subject)
dokwalt deploy --no-cache               # rebuild without the build cache
dokwalt deploy -f compose.yaml -f compose.prod.yaml   # explicit compose files
dokwalt deploy -a blog                  # explicit app
```

Without `-f`, compose's own defaults apply (`compose.yaml`,
`docker-compose.yml`). `-f` is repeatable.

## What happens, step by step

On your Mac:

1. **Read the compose file** with
   `docker compose config --no-interpolate --format json`. Your local
   `.env` is not applied: `${VAR}` values come from the server config.
2. **Build** services that have `build:` for the server's platform
   (`linux/arm64` for a Pi, `linux/amd64` for most VPSs), with
   `BUILDX_NO_DEFAULT_ATTESTATIONS=1` so identical builds give identical
   image IDs. Each image is tagged `dokwalt/<app>-<service>:<id12>` (the
   first 12 hex characters of its image ID). Services built from the same
   Dockerfile share one image.
3. **Capture the git sha** (`-dirty` if there are uncommitted changes).
4. **Ask the daemon which images it already has** and skip them.
5. **Upload** the rest: `docker save --platform linux/<arch>`, compressed
   with zstd, streamed over the SSH connection, loaded by the daemon.
   Unchanged code means `Images unchanged — nothing to upload`.
6. **Send the deploy request** with the normalized model. From here the
   daemon owns the deploy.

On the server (under the server-wide operation lock):

7. **Validate**: transform the model for this stage (see
   `dokwalt docs compose`) and check that every image exists. An invalid
   compose file stops here and does not use up a release number.
8. **Create release vN** with status `deploying`. The release stores the
   normalized model exactly as sent; it is transformed again at every
   rollout, so rollback and promote re-transform for their target stage.
9. **Prepare**: ensure Caddy runs, ensure the stage network
   `dw-<app>-<stage>`, attach Caddy to it, create named volumes.
10. **Data project**: `docker compose up -d --remove-orphans`, then wait
    until ready (up to 3 min). Stateful containers are only recreated if
    their definition changed.
11. **New color** (blue ↔ green): `up -d --remove-orphans`, wait until
    ready including one-shot jobs exiting 0 (up to 3 min).
12. **HTTP health check** of public services (see below).
13. **Switch** Caddy to the new color in one atomic config load. The
    release becomes `succeeded`, older ones `superseded`.
14. **Drain** the old color for 10 s (in-flight requests finish), then
    `docker compose down`. Named volumes are never removed.
15. **Prune** images: keep those of the last 5 successful releases per
    stage. Registry images (e.g. `postgres`) are not pruned.

Registry images without `build:` get `pull_policy: missing`: pulled on
the server if absent, referenced by tag (not pinned by digest).

## Sample output

```text
$ dokwalt deploy -m "new homepage"
◆ Deploying blog  to pi (linux/arm64)
✓ Built migrate, web, worker 23.4s
✓ Uploaded 12.8 MiB in 1.9s
! web: published ports removed — traffic reaches services only through the proxy (use `dokwalt domains:add` or `dokwalt db:connect`)
✓ Services — db: stateful (named volume pgdata), migrate: one-shot job, web: stateless, worker: stateless
✓ Created release v8
✓ Stateful services ready 3.1s
✓ Containers running 9.8s
✓ web is healthy
✓ v8 is live
✓ Previous version stops in 10s (in-flight requests finish)

✓ blog is live at https://blog.example.com
```

## Migrations: one-shot jobs (release phase)

Declare migrations as a service that others depend on with
`condition: service_completed_successfully`:

```yaml
services:
  migrate:
    build: .
    command: ["./bin/migrate", "up"]
    depends_on:
      db: { condition: service_healthy }
  web:
    build: .
    depends_on:
      migrate: { condition: service_completed_successfully }
  db:
    image: postgres:17-alpine
    volumes: [pgdata:/var/lib/postgresql/data]
volumes:
  pgdata:
```

`migrate` becomes a one-shot job: `restart: "no"`, label
`dokwalt.oneshot=true`. It runs in the **new color** before `web` starts,
on **every** release (deploys, config changes, rollbacks, promotions).
A non-zero exit fails the deploy and the old version keeps serving. The
`migrate → db` dependency crosses the data/color split and is dropped;
the data project is always started and ready first anyway. The reconciler
never restarts a job, `dokwalt restart` skips it and it triggers no
crash-loop alert. `dokwalt ps` shows it as `✓ completed`.

## When a deploy fails

Any failure before or at the switch removes the new color. The old
version never stops serving. The release is marked `failed`, a
"Deploy failed" alert is sent, and the last 30 log lines of the failing
container are shown:

```text
✓ Created release v9
✓ Stateful services ready
• Starting v9 (blue)
! Last log lines of web:
  | web Error: connect ECONNREFUSED 10.0.3.4:6379
• Stopping the new version; the current version keeps serving
✗ service web is crash-looping
```

Failed releases stay in `dokwalt releases` with the error as description.

## Readiness and health checks

Before the switch, every container of the new color must be ready:
running and, if it has a compose `healthcheck:`, `healthy`; one-shot jobs
must have exited 0. Unhealthy, exited or restarting fails the deploy.
Then a **3-second stability check**: no container may restart.

Public stateless services (with a non-redirect domain) then get an HTTP
probe per container, `GET http://<container-ip>:<domain port><path>`,
every second until the timeout:

| Check                     | Path     | Passes when      | Timeout             |
|---------------------------|----------|------------------|---------------------|
| default                   | `/`      | any status < 500 | 60 s                |
| `healthcheck:set --path`  | the path | 2xx or 3xx       | `--timeout` (60 s)  |

Services without a domain and without a compose healthcheck only need to
be running and stable. Set a stricter check (applies from the next
release):

```bash
dokwalt healthcheck:set --service web --path /healthz
dokwalt healthcheck:set --service web --path /healthz --timeout 120s
dokwalt healthcheck:unset --service web       # back to GET / < 500
```

## Deploying and going offline

The build and upload need your Mac. Once the deploy request is sent, the
daemon finishes alone: closing the laptop or losing Wi-Fi does not abort
it. Check the result later with `dokwalt releases`. If the upload is
interrupted, run `dokwalt deploy` again; images already on the server
are skipped.

## One operation at a time

Deploys, rollbacks, promotions, config changes and restarts share one
server-wide lock. A second operation, for any app, waits for the first
to finish and then runs. It never fails with "deploy in progress".

## Overlap: workers and migrations

Both colors run side by side for about 10 to 15 seconds, and old code
serves until the switch.

- **Workers** of both colors run at the same time. Jobs must tolerate
  two consumers (locks or idempotent jobs).
- **Migrations** must be backward compatible: the old version keeps
  serving against the migrated schema. Add columns first, remove them in
  a later deploy (expand/contract).

## Other ways to create a release

| Command                  | Build | Upload | Effect                               |
|--------------------------|-------|--------|--------------------------------------|
| `deploy`                 | yes   | yes    | new images, current config           |
| `config:set/unset/import`| no    | no     | same images, new config              |
| `rollback [vN]`          | no    | no     | vN's images and model, current config |
| `rollback --with-config` | no    | no     | vN's images, model and config        |
| `promote`                | no    | no     | staging images, production config    |
| `services:set`           | no    | no     | current images, new stateful override |

All of them use the same blue/green engine. `dokwalt restart` is
different: it restarts the active containers one at a time with
`docker restart` (replicas keep serving, jobs skipped) and creates no
release.

## Rollback

```bash
dokwalt rollback          # latest successful release older than the current one
dokwalt rollback v5       # a specific one
```

No confirmation prompt. The new release is described `Rollback to v5`.
If v5's images were pruned it fails with `image ... is not on the
server`: redeploy from source.

See also: `dokwalt docs compose`, `dokwalt docs releases`,
`dokwalt docs config`, `dokwalt docs pipelines`, `dokwalt docs logs`,
`dokwalt docs troubleshooting`.
