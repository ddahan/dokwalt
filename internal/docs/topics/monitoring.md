# Monitoring

*Live and 7-day CPU, memory, network and HTTP metrics for every app, at almost no cost.*

The daemon collects metrics itself: no Prometheus, no agent containers, no
extra database. Three ways to look at them:

| Command            | What you get                                           |
|--------------------|--------------------------------------------------------|
| `dokwalt top`      | live view of the server and every app, all apps at once |
| `dokwalt metrics`  | history as sparklines, including HTTP metrics          |
| `dokwalt`          | the full-screen dashboard                              |

## Live: `dokwalt top`

```text
$ dokwalt top
◆ prod  10:42:07

CPU  ███░░░░░░░░░░░░░░░░░░░░░  11.4%  load 0.42 · 4 cores
MEM  ██████░░░░░░░░░░░░░░░░░░  26.9%  2.1 GiB / 7.9 GiB
DISK █████████░░░░░░░░░░░░░░░  38.0%  89.2 GiB / 234.0 GiB
TEMP 52°C

APP   SERVICE    CPU    MEMORY      NETWORK                   CPU (10 MIN)
blog  web ×2     3.1%   84.2 MiB    ↓12.0 KiB/s ↑40.1 KiB/s   ▂▂▃▂▂▅▃▂▂▂▃▂
      worker ×1  0.4%   61.0 MiB    ↓1.0 KiB/s ↑1.0 KiB/s
      db ×1      0.8%   112.5 MiB   ↓3.0 KiB/s ↑9.0 KiB/s
shop  web ×1     1.2%   190.3 MiB   ↓4.0 KiB/s ↑8.0 KiB/s     ▁▁▂▁▁▁▂▁▁▁▁▁

DOMAIN             REQ/S  REQ (LAST MIN)  P50    P95    5XX
blog.example.com   1.8    108             12 ms  48 ms  0

  q quit · refreshes every 2s
```

- Always shows the whole server. `-a` doesn't filter it.
- Memory shows `used / limit` when the container has a memory limit.
- The `TEMP` line appears on hosts that report a temperature, with the
  Raspberry Pi throttling state in red when `vcgencmd` reports any.
- Without a terminal, or with `--json`, it prints one snapshot and exits.

## History: `dokwalt metrics`

```text
$ dokwalt metrics --since 24h
◆ blog  last 24h

  web
    CPU                        ▁▁▁▂▂▃▅▇▅▃▂▂▂▃▃▂▂▁▁▁▁▁▂▂▃▂▂▁▁▁▁▂▃▅▃▂▁▁▁▁▁▁▂▂▁▁▁▁  now 2.1% · max 18.0%
    memory                     ▅▅▅▅▅▅▆▆▆▆▅▅▅▅▅▅▅▅▅▅▅▅▅▅▅▅▆▆▆▅▅▅▅▅▅▅▅▅▅▅▅▅▅▅▅▅▅▅  now 84.2 MiB · max 96.0 MiB

  blog.example.com
    requests / bucket          ▁▁▂▂▃▄▆▇▅▃▂▂▃▃▂▂▁▁▁▁▁▁▂▃▄▃▂▁▁▁▁▂▃▄▅▄▃▂▁▁▁▁▂▂▁▁▁▁  now 41 · max 512
    p95 latency                ▁▁▁▁▂▂▃▇▂▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁  now 48ms · max 410ms
    5xx errors                 ▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁  now 0 · max 0
```

- In a linked folder (or with `-a`), you see that app's services and
  domains. Elsewhere you get the whole server: host CPU and memory, then
  every service.
- `--since` defaults to `24h`. It takes a duration (`30m`, `1h`, `24h`) or
  whole days (`7d`). The range is split into about 60 buckets of at least
  1 minute. History goes back `metrics_retention_days` (default 7 days).
- `--json` returns the raw points, including 2xx/3xx/4xx/5xx counts and
  p50/p95/p99, for your own graphs.

## The dashboard

Run `dokwalt` with no arguments in a terminal. It refreshes every 2
seconds. Keys:

| Key                | Action                                              |
|--------------------|-----------------------------------------------------|
| `↑↓` / `j k`       | select an app (or a release when that list is focused) |
| `tab`              | switch focus between the list and the detail pane   |
| `1 2 3` / `← →`    | overview · logs · releases                          |
| `l`                | live logs                                           |
| `r`                | roll back (to the selected release in Releases, else the previous one) |
| `p`                | promote staging to production                       |
| `R`                | restart                                             |
| `o`                | open the site in the browser                        |
| `?` / `q`          | help / quit                                         |

Rollback, promote and restart ask for confirmation with `y`. You can't
deploy from the dashboard: use `dokwalt deploy`.

## What is collected

**Containers, every 10 seconds**, read directly from cgroup v2 files. That
is cheaper than the Docker stats API, which is only a fallback:

- CPU from `cpu.stat`, memory from `memory.current`/`memory.stat`
- disk read/write from `io.stat`
- network in/out from `/proc/<pid>/net/dev`
- restarts and OOM kills from Docker events

**Host, every 10 seconds**: CPU, memory and load from `/proc`, disk usage
via `statfs`, and the temperature from `/sys/class/thermal`. On a
Raspberry Pi, `vcgencmd get_throttled` runs every 60 seconds and reports
under-voltage, frequency capping, throttling and the soft temperature limit,
both now and since boot.

**HTTP, per domain, per minute**: Caddy writes its JSON access log to a Unix
socket read by the daemon, with nothing on disk. The daemon counts
requests and 2xx/3xx/4xx/5xx, and computes p50/p95/p99 latency from
histogram buckets.

Memory metrics need the cgroup memory controller. If it is disabled (some
kernels built for ARM boards), `dokwalt doctor` says so: add
`cgroup_enable=memory cgroup_memory=1` to the kernel command line and reboot
(`dokwalt docs server`).

## Storage and retention

| Data                  | Where                                   | Kept                  |
|-----------------------|-----------------------------------------|-----------------------|
| live 10 s samples     | daemon memory                           | recent minutes        |
| 1-minute aggregates   | SQLite `/var/lib/dokwalt/dokwalt.db`    | 7 days (pruned hourly) |
| individual requests   | not stored                              | —                     |

The retention is the server setting `metrics_retention_days` (default 7).
There is no CLI command for it yet.

## Overhead

Measured on a test server: the daemon uses about **27 MiB RSS**, of which
about 9 MiB is heap (the rest is shared binary code). Caddy uses about
**46 MiB RSS**, with about 12 MiB of heap. That makes about 73 MiB in
total, under the 100 MB target. Check your own numbers:

```text
$ dokwalt server info
◆ prod  deploy@203.0.113.10

Host            prod (linux/arm64), up 12d
DokWalt         0.1.0 (API 1.0) · CLI 0.1.0
Docker          28.4.0 · compose 2.39.1
Apps            2
Resources       4 CPUs · 2.1 GiB / 7.9 GiB RAM · 89.2 GiB / 234.0 GiB disk · 52°C
Footprint       daemon 27.1 MiB · proxy 46.3 MiB  (heap 9.2 MiB + 12.0 MiB; the rest is shared binary code)
Let's Encrypt   you@example.com
```

How it stays small:

- **Transparent huge pages off.** With THP set to `always` (a common
  default), the kernel backs the Go heap with 2 MiB pages, which inflates
  RSS several times. The daemon disables THP for itself at startup
  (`prctl PR_SET_THP_DISABLE`, then re-exec). Caddy runs with
  `GODEBUG=disablethp=1`.
- **Memory limits for the Go runtime**: `GOMEMLIMIT=40MiB` for the daemon
  (systemd unit), `GOMEMLIMIT=96MiB` for Caddy.

`dokwalt doctor` shows the daemon's RSS and warns above 60 MiB.

## Turning metrics into alerts

Disk, memory, temperature and crash-loop thresholds can notify Discord or
Slack. Site down, deploy failed, certificate errors, out of memory,
self-healing failed and Pi throttling are always on:

```bash
dokwalt alerts:add discord https://discord.com/api/webhooks/…
dokwalt alerts:set disk=85 temperature=75
```

## Quick recipes

```bash
dokwalt top                                  # everything, live
dokwalt metrics --since 1h                   # this app, the last hour
dokwalt -a shop metrics --since 7d --json > week.json
dokwalt metrics -s staging --since 24h       # staging (pipeline apps)
```

See also: `dokwalt docs alerts`, `dokwalt docs logs`,
`dokwalt docs server`, `dokwalt docs troubleshooting`
