# Logs

*Stream, filter and search your app's logs from your computer, and know where every other log lives.*

Your services log to stdout/stderr (12-factor). DokWalt reads container logs
through the Docker API on the server and streams them to your terminal,
merged across all services of the app stage.

## Basics

```bash
dokwalt logs              # last 100 lines, all services, merged by time
dokwalt logs -f           # then keep following (Ctrl+C to stop)
dokwalt logs -a blog -f   # outside a linked folder
```

```text
$ dokwalt logs -f
10:42:01 web    │ GET /posts/hello 200 12ms
10:42:02 web.2  │ GET /feed.xml 200 4ms
10:42:05 worker │ sent newsletter batch 3/10
10:42:07 db     │ LOG:  checkpoint complete: wrote 42 buffers
10:42:09 web    │ GET /admin 302 1ms
```

Each line shows the local time, the service (`web.2` for the second
replica) and the line. Each service gets its own color, stable across runs.
Colors are dropped when output isn't a terminal (pipes, files).

**Which containers**: without `-f`, the stage's active containers (the
data project and the live color). With `-f`, DokWalt also attaches to new
containers as they appear, so during a deploy you see the new color start.

## Flags

| Flag                    | Meaning                                                   |
|-------------------------|-----------------------------------------------------------|
| `-f, --follow`          | keep streaming new lines                                  |
| `--service <svc>`       | only this service                                         |
| `--grep <text>`         | only lines containing the text (case-insensitive)         |
| `-n, --lines <n>`       | number of past lines, across all services (default 100)   |
| `--since <duration>`    | every line since that long ago, e.g. `30m`, `2h`, `36h`   |
| `-s staging`            | the staging stage (pipeline apps only)                    |
| `--json`                | one JSON object per line (NDJSON)                         |

- `--grep` is a plain **substring**, not a regex, matched case-insensitively
  on the server, so filtered-out lines never cross the network. Matches are
  highlighted.
- `--since` takes a **duration** only: `s`, `m`, `h` units, combinable
  (`1h30m`). No dates or RFC3339 timestamps, and no `d` unit: use `48h`.
  With `--since`, all lines in that window are shown and `-n` is ignored.

## Examples

```bash
# only the web service, following
dokwalt logs --service web -f

# errors in the last 2 hours (matches error, Error, ERROR…)
dokwalt logs --since 2h --grep error

# a longer history for the worker
dokwalt logs --service worker -n 1000

# staging, following 5xx responses
dokwalt logs -s staging --service web --grep " 50" -f

# regex or counting: pipe into local tools
dokwalt logs --since 168h --service web | grep -cE "POST /(login|signup)"
```

## JSON output

```text
$ dokwalt logs --service web -n 2 --json
{"time":"2026-09-27T10:42:01Z","service":"web","replica":1,"stream":"stdout","line":"GET /posts/hello 200 12ms"}
{"time":"2026-09-27T10:42:02Z","service":"web","replica":2,"stream":"stdout","line":"GET /feed.xml 200 4ms"}
```

Pipe it into `jq`:

```bash
dokwalt logs --since 1h --json | jq -r 'select(.stream=="stderr") | .line'
```

## Deploy logs

`dokwalt deploy` streams its progress live: the local build, the image
upload, then each step on the server.

```text
$ dokwalt deploy -m "fix feed"
◆ Deploying blog  to prod (linux/arm64)
✓ Built migrate, web for linux/arm64
✓ Uploaded 16.6 MiB in 0.6s
✓ Services — db: keeps its data (named volume pgdata), migrate: runs once per deploy, web: zero-downtime swap
✓ Created release v14
✓ Stateful services unchanged
✓ Starting v14 next to the current version · containers running
✓ web is healthy
✓ v14 is live
✓ Previous version stops in 10s (in-flight requests finish)
✓ blog is live at https://blog.example.com
```

When a deploy fails, the last 30 log lines of the failing container are
streamed (`Last log lines of <svc>:`), and the error is kept on the release
(`dokwalt releases`). `promote` and `rollback` stream the same way. Without
a terminal the steps print as plain lines. If you close the laptop after
the upload, the deploy continues on the server.

## Rotation and retention

- DokWalt sets the Docker `local` log driver with **10 MB × 3 files per
  container**, unless the service defines its own `logging:` in compose.
- A container's logs live as long as the container. After a deploy the
  previous color's containers are removed, and their logs with them.
  Stateful services keep their logs across deploys unless their definition
  changes and the container is recreated.
- To keep logs longer, ship them from your app to an external service.

## Other logs

**HTTP access logs.** Caddy sends its access and TLS logs to a Unix socket
read by the DokWalt daemon. Nothing is written to disk. The daemon turns
them into per-host, per-minute metrics: requests, status classes and
latency percentiles. Individual requests aren't stored: see
`dokwalt metrics` and `dokwalt docs monitoring`. Log requests in your app
if you need each one.

**Caddy errors** (certificates, config loads), on the server:

```bash
ssh prod docker logs --since 1h dokwalt-caddy
```

**The DokWalt daemon** runs under systemd, so its log is in the journal:

```bash
ssh prod journalctl -u dokwalt -f
ssh prod journalctl -u dokwalt --since "1 hour ago"
ssh prod journalctl -u dokwalt -b     # since the last boot
```

The daemon logs reconciliation (what it repaired after a reboot), deploy
steps, pruned images, alerts (also when no channel is configured), failed
alert deliveries and errors.

## Debugging a crashing service

```bash
dokwalt ps                                     # state, health, restarts
dokwalt logs --service worker --since 15m      # what happened before it died
dokwalt doctor                                 # stage status, disk, memory
```

See also: `dokwalt docs deploy`, `dokwalt docs monitoring`,
`dokwalt docs troubleshooting`, `dokwalt docs reboot-recovery`
