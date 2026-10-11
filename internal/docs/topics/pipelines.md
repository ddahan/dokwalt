# Pipelines: staging and promote

*Optional per-app staging stage, with instant promotion of the exact images you tested.*

## Single stage by default

Every app starts with one stage, `production`, and it is invisible: you
never pass a stage, and no output mentions one. Small apps pay nothing
for pipelines. `-s staging` fails on apps without a pipeline:

```text
$ dokwalt releases -s staging
✗ app "blog" has no staging stage (enable it with `dokwalt pipeline:enable`): not found
```

## Enable a pipeline

```bash
dokwalt pipeline:enable
```

```text
✓ Pipeline enabled for blog: staging → production
  1. dokwalt domains:add staging.example.com -s staging --service web
  2. dokwalt deploy -s staging
  3. dokwalt promote  (same images, production config, no rebuild)
```

Production keeps running untouched (no redeploy). Staging runs on the
**same server**, fully separate:

| Thing        | production                     | staging                     |
|--------------|--------------------------------|-----------------------------|
| Projects     | `dw-blog-production-*`         | `dw-blog-staging-*`         |
| Network      | `dw-blog-production`           | `dw-blog-staging`           |
| Volumes      | `dw-blog-production-pgdata`    | `dw-blog-staging-pgdata`    |
| Config       | its own                        | its own                     |
| Domains      | `blog.example.com`             | `staging.blog.example.com`  |
| Overrides    | its own                        | its own                     |
| Releases     | v1, v2, ...                    | v1, v2, ...                 |

Each stage has its own private network, with Caddy attached to both.
Staging containers cannot reach production containers (and the reverse),
and staging's database lives in its own volume.

Once the pipeline is on, output shows the stage for staging
(`blog (staging)`), and commands without `-s` target production.

## Configure staging

```bash
dokwalt config:set -s staging NODE_ENV=staging DB_PASSWORD=staging-pw
dokwalt config -s staging
dokwalt domains:add staging.blog.example.com -s staging --service web --port 3000
```

Before staging's first deploy, `domains:add` needs `--service` (the port
defaults to the service's declared port). Create the DNS record for
`staging.blog.example.com` first. Use a separate domain per stage;
wildcards are not supported.

## Deploy to staging

```bash
dokwalt deploy -s staging -m "new checkout flow"
```

```text
◆ Deploying blog (staging)  to prod (linux/arm64)
✓ Built migrate, web, worker
✓ Uploaded 12.8 MiB in 1.9s
✓ Services — db: keeps its data (named volume pgdata), migrate: runs once per deploy, web: zero-downtime swap, worker: zero-downtime swap
✓ Created release v14
✓ Stateful services ready
✓ Containers running
✓ web is healthy
✓ v14 is live
✓ Previous version stops in 10s (in-flight requests finish)

✓ blog (staging) is live at https://staging.blog.example.com
```

Test it at `https://staging.blog.example.com`, look at
`dokwalt logs -s staging -f`, run `dokwalt db:connect -s staging`.

## Promote to production

```bash
dokwalt promote
```

```text
◆ Promoting blog  staging → production
✓ Promoting staging v14 to production (same images, production config)
✓ Services — db: keeps its data (named volume pgdata), migrate: runs once per deploy, web: zero-downtime swap, worker: zero-downtime swap
✓ Created release v31
✓ Stateful services ready
✓ Containers running
✓ web is healthy
✓ v31 is live
✓ Previous version stops in 10s (in-flight requests finish)
✓ Promoted — production is now v31
```

There is no confirmation prompt in the CLI (the dashboard asks with `y`).
What promote does:

- Takes the images and the stored compose model of **staging's current
  release**: the built images are the very same content-addressed tags
  you tested. Registry images are referenced by tag and reused from the
  server's local image store.
- Re-transforms that model for production and uses **production's
  config, overrides and domains**, never staging's.
- **No build, no upload**: the images are already on the server. It
  takes as long as the rollout and health checks.
- Creates a normal production release, described
  `Promote staging v14: new checkout flow`, with the same blue/green
  switch, one-shot jobs, health checks and automatic abort on failure.

Promote only reads staging; it never changes it. It fails if staging has
no release yet (`deploy to staging first`).

## Rollback per stage

Each stage has its own release history:

```bash
dokwalt releases                 # production
dokwalt releases -s staging
dokwalt rollback                 # production to its previous release
dokwalt rollback -s staging v12  # staging to v12
```

A bad promotion is undone with `dokwalt rollback` (no prompt): production
goes back to its previous images, staging keeps what it has.

## Typical workflow

```bash
dokwalt deploy -s staging -m "feature X"   # build + ship to staging
# test https://staging.blog.example.com
dokwalt promote                            # same images, prod config
dokwalt rollback                           # if production misbehaves
```

In the dashboard (`dokwalt`), select an app and press `p` to promote,
then `y` to confirm.

## Disable the pipeline

```bash
dokwalt pipeline:disable
dokwalt pipeline:disable --confirm blog    # non-interactive
```

```text
! This deletes the staging stage of blog: containers, volumes, config and domains.
  Type blog to confirm: blog
✓ Pipeline disabled for blog
```

Staging's containers, **volumes**, network, config, domains and releases
are deleted; production keeps running untouched and the stage disappears
from output again. If staging's database holds anything you need, dump it
first with `dokwalt db:connect -s staging --tunnel-only`.

See also: `dokwalt docs deploy`, `dokwalt docs releases`,
`dokwalt docs config`, `dokwalt docs domains`, `dokwalt docs concepts`.
