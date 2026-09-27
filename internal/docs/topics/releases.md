# Releases & rollback

*Every deploy, config change, promote and rollback creates an immutable, numbered release you can return to.*

## What a release is

A release is a frozen snapshot of what runs, numbered per stage: `v1`,
`v2`, `v3`… (production and staging each have their own numbers).

| Part          | Content                                                        |
|---------------|----------------------------------------------------------------|
| images        | each built service → `dokwalt/<app>-<svc>:<12 hex of image ID>` |
| compose       | your normalized compose model, as sent by the CLI              |
| config        | the config version, shown as `c<N>` (values are not copied)    |
| description   | your `deploy -m` message, the last commit subject, or automatic |
| git sha       | short commit of the folder you deployed from (`-dirty` if uncommitted changes) |
| color         | `blue` or `green`                                              |
| status, error | see below                                                      |
| created       | timestamp                                                      |

The compose model is stored as you wrote it (without interpolation), and
it is transformed for the target stage at deploy time. That is why a
promote or rollback gets the right networks, volumes and domains.

Registry images (e.g. `postgres:17-alpine`) are referenced **by tag**, not
pinned by digest. They are pulled only if missing on the server.

Releases are never edited: any change creates a new one.

## What creates a release

| Action                          | New release contains                       | Description               |
|---------------------------------|--------------------------------------------|---------------------------|
| `dokwalt deploy`                | new images + compose, current config       | `-m` or commit subject    |
| `config:set` / `unset` / `import` | same images + compose, new config version | `Set SMTP_HOST, SMTP_PORT` |
| `dokwalt rollback v12`          | v12's images + compose, current config     | `Rollback to v12`         |
| `dokwalt promote`               | staging's current images + compose, production config | `Promote staging v8: …` |
| `services:set --stateful=…`     | same images + compose, new override        | `Service cache stateful=false` |

A config change doesn't create a release with `config:set --no-restart`, or
if the stage was never deployed. It applies at the next release.

Each release goes through the same zero-downtime flow: start the new color,
run one-shot jobs, wait for readiness and the health check, switch
traffic, then drain and stop the old color.

`restart`, `stop`, `start`, domain changes and `healthcheck:set` do **not**
create releases. A compose file that fails validation is rejected before
any release is created, so it doesn't use up a version number.

## Statuses

| Status        | Meaning                                                        |
|---------------|----------------------------------------------------------------|
| `deploying`   | created, containers starting / checks running                  |
| `succeeded`   | passed its checks and received traffic                         |
| `failed`      | aborted before the switch: the previous release stayed live and a *Deploy failed* alert was sent |
| `superseded`  | succeeded earlier, then replaced by a newer release            |

There is no pending status: a release is created when its deploy starts.
The release currently serving traffic is shown as `▶ vN` with status `live`.

## Listing releases

```text
$ dokwalt releases
RELEASE  STATUS         DESCRIPTION                                   COMMIT        CONFIG  CREATED
▶ v16    ● live         Rollback to v14                               a1b2c3d       c7      3m ago
  v15    ● failed       web did not pass its health check             9f3e2a1       c7      12m ago
  v14    ● superseded   fix feed                                      a1b2c3d       c7      2d ago
  v13    ● superseded   Set SMTP_HOST                                 77d0c4e       c7      2d ago
  v12    ● superseded   new theme                                     77d0c4e-dirty c6      5d ago
```

For a failed release, the first line of the error replaces the description.
`-n/--limit` sets how many releases are shown (default 15).
`--json` returns the full records, including images and the complete error.
With a pipeline: `dokwalt releases -s staging`.

## Rolling back

```bash
dokwalt rollback          # to the latest successful release before the current one
dokwalt rollback v12      # to a specific release
```

Without an argument, the target is the most recent `succeeded` or
`superseded` release older than the current one. The CLI asks **no
confirmation** (the dashboard's `r` key asks with `y`).

```text
$ dokwalt rollback v12
◆ Rolling back blog
✓ Services — db: stateful (named volume pgdata), web: stateless
✓ Created release v17
✓ Stateful services unchanged
✓ Starting v17 (blue) · containers running
✓ web is healthy
✓ v17 is live
✓ Previous version stops in 10s (in-flight requests finish)
✓ Rolled back — v17 is live
```

A rollback **creates a new release** (`v17`, "Rollback to v12"). It doesn't
reactivate v12 itself. It's fast because nothing is built or uploaded: the
images are still on the server. It still runs the normal checks, and
one-shot jobs run again, so a broken target is refused and the current
release stays live.

### Config during rollback

By default a rollback keeps the **current** config. That is usually what
you want: secrets you rotated since v12 stay rotated.

```bash
dokwalt rollback v12 --with-config
```

`--with-config` first restores the config v12 was deployed with, as a new
config version, then deploys. Use it to undo a bad config change:

```text
$ dokwalt config:set CACHE_TTL=0       # v18 (c8), the site gets slow
$ dokwalt rollback --with-config       # v19 = v17's images + v17's config (c9)
```

### What rollback doesn't undo

- **Data**: databases and volumes are never rolled back. Undo migrations
  yourself, and keep them backward compatible so old code works.
- **Domains, redirects, health checks, service overrides, alert
  settings**: they live outside releases.
- **Stateful services**: if the target release's db definition differs
  (e.g. another postgres image), the db container is recreated with that
  definition. Be careful across major versions (`dokwalt docs databases`).

## Retention

After each successful deploy, DokWalt prunes built images. It keeps those
of the **5 most recent successful releases per stage**, always including
the current one (server setting `keep_releases`, which has no CLI command
yet).
Registry images are never pruned. Release records stay in the history, but
rolling back to one whose images are gone fails before any release is
created:

```text
$ dokwalt rollback v9
✗ image dokwalt/blog-web:3f9c2a1b7d0e for service web is not on the server (was it pruned? keep fewer releases or redeploy)
```

Redeploy that version from source instead:
`git checkout <sha> && dokwalt deploy`.

## Promote (pipeline apps)

```text
$ dokwalt promote
◆ Promoting blog  staging → production
✓ Promoting staging v8 to production (same images, production config)
✓ Created release v21
✓ Starting v21 (green) · containers running
✓ web is healthy
✓ v21 is live
✓ Promoted — production is now v21
```

Promote copies the images and compose model of staging's current release
into a new production release, with production's config, overrides and
domains. No build, no upload, and no confirmation prompt in the CLI. To
undo a promote, run `dokwalt rollback` in production.

## Failed releases

A deploy fails if:

- a container exits, restarts or turns unhealthy, or isn't ready within
  3 minutes;
- a one-shot job exits non-zero;
- a public service doesn't pass its HTTP check within its timeout (60 s by
  default). By default any answer below 500 on `/` passes. With
  `healthcheck:set --path`, the answer must be 2xx/3xx;
- a container restarts during the final 3-second stability check.

DokWalt then removes the new color, keeps the old one serving, marks the
release `failed`, streams the last 30 log lines of the failing container
and sends a *Deploy failed* alert.

```bash
dokwalt releases                          # the error is shown on v15
dokwalt logs --service web --since 15m    # what the app printed
```

## Concurrency and interruptions

- **One operation at a time, server-wide**: deploys, rollbacks, promotes and
  config changes share a single lock. A second one **waits** for the first
  to finish, even for another app. There is no "deploy in progress" error.
- **Deploys belong to the daemon**: pressing Ctrl+C or closing the laptop
  after the upload doesn't abort anything. Check the outcome later with
  `dokwalt releases`.
- **Interrupted by a reboot**: a release still `deploying` when the daemon
  starts is marked `failed` and its color torn down. The reconciler keeps
  the previous release running.

See also: `dokwalt docs deploy`, `dokwalt docs config`, `dokwalt docs pipelines`,
`dokwalt docs databases`, `dokwalt docs reboot-recovery`
