# Config and secrets

*Environment variables stored encrypted on the server, versioned with every release (12-factor).*

## The rule

Anything that varies between deploys (secrets, API keys, `NODE_ENV`,
database URLs) lives **only on the server**, managed with the CLI. Your
repository keeps the structure (Dockerfile, compose file) and no values.
Your local `.env` is never read during a deploy.

## Commands

```bash
dokwalt config                        # list, values masked
dokwalt config --reveal               # list with values in clear text
dokwalt config --shell                # KEY="value" lines, clear text
dokwalt config:get DATABASE_URL       # print one raw value
dokwalt config:set NODE_ENV=production LOG_LEVEL=info
dokwalt config:set SMTP_HOST=smtp.example.com --no-restart
dokwalt config:unset LOG_LEVEL DEBUG
dokwalt config:import .env            # load a dotenv file
```

Add `-a blog` outside a linked folder, `-s staging` for a pipeline app's
staging stage, `--json` for scripts. Keys must match
`[A-Za-z_][A-Za-z0-9_]*`.

```text
$ dokwalt config
◆ Config of blog
KEY            VALUE
DATABASE_URL   po••••••••••••
DB_PASSWORD    9f••••••••••••
NODE_ENV       pr••••••••
SMTP_HOST      sm••••••••••••

$ dokwalt config --reveal
◆ Config of blog
KEY            VALUE
DATABASE_URL   postgres://blog:9f2c41d07ab3e5c8@db:5432/blog
DB_PASSWORD    9f2c41d07ab3e5c8
NODE_ENV       production
SMTP_HOST      smtp.example.com
```

Masked values show the first 2 characters followed by `•` (values of 4
characters or fewer are fully masked). `config:get` and `--shell` print
clear text, handy in scripts:

```bash
export DATABASE_URL=$(dokwalt config:get DATABASE_URL)
dokwalt config --shell > .env.production.local    # keep it out of git
```

## Every change is a release

`config:set`, `config:unset` and `config:import` each create a new config
version (`c<N>`) and, if the stage is deployed, a new release with the
**same images and compose model**, rolled out with the usual zero-downtime
blue/green switch (one-shot jobs run again). Several keys in one command
make one release, described `Set K1, K2` or `Unset K`.

```text
$ dokwalt config:set LOG_LEVEL=debug SMTP_HOST=smtp.example.com
✓ Config updated: Set LOG_LEVEL, SMTP_HOST
✓ Services — db: keeps its data (named volume pgdata), web: zero-downtime swap, worker: zero-downtime swap
✓ Created release v9
✓ Stateful services ready
✓ Containers running
✓ web is healthy
✓ v9 is live
✓ Previous version stops in 10s (in-flight requests finish)
✓ Config updated — blog restarted as v9 with zero downtime
```

- `--no-restart` (on `config:set`) only stores the values: "applies on
  next deploy".
- Before the first deploy, changes are just stored: "applies on first
  deploy".
- If the new config breaks the app (a health check fails), the release is
  marked failed and the previous one keeps serving.
- Config changes wait for any running operation (one server-wide lock).

## Importing a .env file

```bash
dokwalt config:import .env
dokwalt config:import .env.production -s staging
```

Dotenv syntax: `KEY=value`, an optional `export ` prefix, `# comments`,
blank lines, single- or double-quoted values, and ` # comment` after an
unquoted value. Double-quoted values understand escapes such as `\n`.
Each line is one variable. Imported keys are added or overwritten; keys
not in the file are kept. Delete the file afterwards if it holds
production secrets you do not need locally.

## Multi-line values

Certificates, private keys and JSON blobs work. Let the shell read the
file and quote the whole argument:

```bash
dokwalt config:set "JWT_PRIVATE_KEY=$(cat jwt.pem)"
dokwalt config:set "GOOGLE_CREDENTIALS=$(cat service-account.json)"
```

In a dotenv file, keep the value on one line and write line breaks as
`\n` inside double quotes.

## How config reaches containers

| Mechanism     | Where                                   | Services           |
|---------------|-----------------------------------------|--------------------|
| Interpolation | `${VAR}` anywhere in the compose file   | all services       |
| Pass-through  | every key added as `KEY: null`          | stateless services |

```yaml
services:
  web:
    build: .                     # gets every key: DATABASE_URL, ...
  db:
    image: postgres:17           # stateful: no pass-through
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}   # interpolated on the server
```

**Values are never written to disk.** The rendered compose files under
`/var/lib/dokwalt/apps/` only hold `KEY: null` entries and literal
`${VAR}` references. The daemon runs `docker compose` with the decrypted
values in that process's environment, and compose reads both the
pass-through entries and the interpolation from there.

Consequences:

- **A stateful service only sees what you interpolate.** Pass database
  settings through `${VAR}` in its `environment:`.
- **Config changes do not restart databases.** The data project is only
  recreated when its definition changes.
- **Changing a variable used in a stateful definition does recreate
  that container** at the next release (for `db` above: `DB_PASSWORD`).
  Expect a short database interruption.
- A config key **overrides** an `environment:` entry of the same name in
  stateless services.
- Postgres, MySQL and similar images only read `POSTGRES_*` or `MYSQL_*`
  when the volume is first initialized. Changing `DB_PASSWORD` later does
  not change the real password: change it inside the database first
  (`dokwalt db:connect`), then update config.
- `${VAR}` is resolved on the server, never from your computer. Defaults such
  as `${LOG_LEVEL:-info}` work as in Compose; a missing variable without
  default warns during deploy and becomes empty.

## Encryption at rest

Values are encrypted with XChaCha20-Poly1305 in the daemon's SQLite
database. The key is `/var/lib/dokwalt/secret.key` (32 random bytes, mode
0600, created by the daemon at first start). Values are masked in output
unless you pass `--reveal` or `--shell`.

Back up `secret.key` together with `dokwalt.db`: without the key the
config cannot be decrypted. See `dokwalt docs security`.

Containers still receive plain environment variables; anyone with root
or `docker` group access on the server can read them (`docker inspect`).
That is inherent to env-var config.

## Config versions and releases

Each release records the config version it ran with:

```text
$ dokwalt releases
RELEASE   STATUS         DESCRIPTION                COMMIT    CONFIG   CREATED
▶ v9      ● live         Set LOG_LEVEL, SMTP_HOST   3f2c1ab   c6       1m ago
  v8      ● superseded   new homepage               3f2c1ab   c5       2h ago
  v7      ● superseded   deploy                     d41e0b2   c4       1d ago
```

Rollback keeps the **current** config by default, so it never resurrects
an old secret you rotated:

```bash
dokwalt rollback v7                 # v7 images, config c6
dokwalt rollback v7 --with-config   # v7 images, v7's config restored as c7
```

Use `--with-config` when the config change itself was the problem.

## Exporting config

`dokwalt apps:export blog.json` writes the app's server-side settings,
including all config **in clear text**, to a file with mode 0600 and
prints a warning. Without a file name it prints the JSON to stdout. Keep
it out of git and delete it once you have run
`dokwalt apps:import blog.json` on the new server.

## Pipelines

With a pipeline enabled, staging and production have fully separate
config. Promotion uses production's config, never staging's:

```bash
dokwalt config -s staging
dokwalt config:set -s staging NODE_ENV=staging
```

See also: `dokwalt docs compose`, `dokwalt docs deploy`,
`dokwalt docs releases`, `dokwalt docs pipelines`,
`dokwalt docs security`, `dokwalt docs databases`.
