# Alerts

*Get a Discord or Slack message when a site goes down, a deploy fails or the server runs hot.*

Alerts are evaluated and sent by the daemon on the server, so they work
while your laptop is closed. Channels and thresholds apply to the whole
server (all apps). With no channel configured, alerts are only written to
the daemon log (`journalctl -u dokwalt`).

## 1. Create a webhook URL

**Discord**

1. Open the server settings → **Integrations** → **Webhooks**.
2. **New Webhook**, give it a name (e.g. `DokWalt`), choose the channel.
3. **Copy Webhook URL**:
   `https://discord.com/api/webhooks/<id>/<token>`

**Slack**

1. Go to `https://api.slack.com/apps` → **Create New App** → *From scratch*,
   pick your workspace.
2. **Incoming Webhooks** → turn **Activate Incoming Webhooks** on.
3. **Add New Webhook to Workspace**, choose the channel, **Allow**.
4. Copy the URL: `https://hooks.slack.com/services/T…/B…/…`

Treat webhook URLs as secrets: anyone who has one can post to your channel.
DokWalt hides everything after the host when listing them. They are stored
unencrypted in the server's database.

## 2. Add the channel and test it

```text
$ dokwalt alerts:add discord https://discord.com/api/webhooks/1234/abcd
✓ Added discord channel #1
  Try it: dokwalt alerts:test

$ dokwalt alerts:test
✓ Test alert sent — check your channel(s)
```

The channel receives **👋 DokWalt test alert**, followed by
`Alerts from <hostname> reach this channel.` The kind must be `discord` or
`slack`, and the URL must start with `https://`. If a delivery fails, the
command reports each channel, e.g.
`✗ discord #1: webhook answered HTTP 404`.

You can add several channels (e.g. Discord and Slack), and every alert goes
to all of them:

```bash
dokwalt alerts:add slack https://hooks.slack.com/services/T0/B0/xyz
```

## 3. Review the setup

```text
$ dokwalt alerts
◆ Alerts on prod

ID  CHANNEL  WEBHOOK
1   discord  https://discord.com/•••
2   slack    https://hooks.slack.com/•••

Disk          90% used
Memory        90% used
Temperature   80°C
Crash loop    3 restarts / 10 min
Always on     site down · deploy failed · certificate errors · out of memory · throttling

✗ site:blog.example.com
```

The last part lists the alert keys currently firing, or `✓ Nothing firing`.

## 4. Tune thresholds

```bash
dokwalt alerts:set disk=85 memory=95
dokwalt alerts:set temperature=75 restarts=5
dokwalt alerts:set restarts=0          # no crash-loop alerts
```

| Key           | Meaning                                         | Default |
|---------------|-------------------------------------------------|---------|
| `disk`        | disk used on the server, in %                   | `90`    |
| `memory`      | memory used on the server, in %                 | `90`    |
| `temperature` | CPU temperature, in °C                          | `80`    |
| `restarts`    | deaths of one service within 10 minutes         | `3`     |

These are the only four keys, and values must be numbers. Any other key
(there is no `throttling` key) is refused with
`unknown alert setting … (known: disk, memory, temperature, restarts)`.

Setting any of them to `0` disables that rule (e.g. `temperature=0` on a VPS
without a sensor). Otherwise a rule fires when the value is **≥** the threshold.

## The rules

| Rule                | Triggers when                                                   | Resolves when                |
|---------------------|-----------------------------------------------------------------|------------------------------|
| Site down           | 2 consecutive failed probes of a domain (error or 5xx)          | the next probe succeeds      |
| Deploy failed       | a deploy, config release, rollback or promote fails             | the next release succeeds    |
| Certificate error   | Caddy couldn't get a certificate for a domain                   | the certificate is obtained  |
| Crash loop          | a service dies ≥ `restarts` times in 10 minutes                 | 10 minutes without a death   |
| Out of memory       | Docker reports an OOM kill in one of your containers            | —                            |
| Self-healing failed | the reconciler can't restore a stage                            | a later pass succeeds        |
| Disk                | disk usage ≥ `disk` %                                           | below the threshold          |
| Memory              | memory usage ≥ `memory` %                                       | below the threshold          |
| Temperature         | CPU temperature ≥ `temperature` °C                              | below the threshold          |
| CPU throttling      | the board reports under-voltage or throttling **now**           | the condition clears         |

Site down, deploy failed, certificate error, out of memory, self-healing
failed and throttling are **always on**. Disk, memory and temperature are
checked every minute. Crash loops count Docker `die` events, and one-shot
jobs (migrations) are excluded.

**How site down is probed.** Every 60 seconds the daemon connects to
`127.0.0.1:443` with the domain as SNI and requests `GET /`. The
certificate is **not** validated: the probe checks that your app answers.
Certificate problems have their own alert. Going through localhost avoids
hairpin-NAT problems on home networks. It doesn't test DNS or your router:
`dokwalt doctor` and `dokwalt domains` do. Stopped, deploying and
never-deployed stages are skipped, and so are redirect domains.

## Rate limiting and resolved messages

- At most **one message per alert key every 30 minutes** while it is
  firing. The key is e.g. `site:<host>`, `deploy:<app>/<stage>`,
  `restarts:<app>/<stage>/<svc>` or `host:disk`. A site that stays down
  sends one message, then a reminder every 30 minutes at most.
- When the condition clears, one `✅ Resolved: <title>` message is sent.

## Message format

Discord (posted as user `DokWalt`):

```text
**🔴 <title>** — `<hostname>`
<message>
```

Slack:

```text
*🔴 <title>* — `<hostname>`
<message>
```

Examples as they appear in the channel:

```text
🔴 Site down — prod
blog.example.com is not answering: HTTP 502

✅ Resolved: Site down — prod
blog.example.com answers again

🔴 Deploy failed — prod
blog/production v15 failed: web did not pass its health check

🔴 Crash loop — prod
blog/production/worker restarted 4 times in 10 minutes (exit code 1). `dokwalt logs -a blog`

🔴 Out of memory — prod
blog/production/worker was killed: out of memory

🔴 Certificate error — prod
Could not get a certificate for api.example.com: …
Run `dokwalt doctor` to check DNS and ports.

🔴 Self-healing failed — prod
blog/production: …

🔴 Disk almost full — prod
Disk is 91% full (threshold 90%). Try `docker system prune` or remove old apps.

🔴 Memory pressure — prod
Memory is 93% used (threshold 90%).

🔴 CPU temperature high — prod
CPU at 81°C (threshold 80°C). Check cooling.

🔴 CPU throttling — prod
Raspberry Pi reports: now: under-voltage. Check power supply and cooling.
```

A deploy failure resolves with `✅ Resolved: Deploy succeeded` and
`<app>/<stage> vN is live`.

## Removing a channel

```bash
dokwalt alerts                 # find the id
dokwalt alerts:remove 2
```

With no channel left, alerts are only logged on the server. Thresholds are
kept.

## Troubleshooting

- `alerts:test` reports HTTP 401/404: the webhook was deleted or the URL is
  incomplete. Create a new one, `alerts:remove` the old id, then
  `alerts:add` the new one.
- No messages at all: check the server can reach the internet
  (`ssh prod curl -I https://discord.com`) and read the daemon log, where
  failed deliveries are logged:
  `ssh prod journalctl -u dokwalt --since "1 hour ago"`.

See also: `dokwalt docs monitoring`, `dokwalt docs domains`,
`dokwalt docs releases`, `dokwalt docs server`
