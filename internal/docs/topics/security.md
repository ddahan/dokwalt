# Security

*DokWalt's threat model and how access, secrets, network exposure and containers are protected.*

## Threat model

DokWalt is a single-admin tool: one person, one or a few servers, many sites.

| It protects against | It does not protect against |
|---|---|
| Internet scanners and attackers reaching the server | Someone who can log in over SSH as you (that is root) |
| Secrets leaking through the repository | Malicious images you choose to deploy |
| Databases and internal ports exposed by accident | Physical access to an unencrypted disk |
| A copied `dokwalt.db` or backup revealing config values | A compromised Mac with an unlocked SSH agent |
| One app reaching another app (database or web service) | Vulnerabilities inside your own applications |

The whole design reduces to two doors: **SSH (22)** for you, **Caddy (80/443)** for visitors.

## Access control

- **Authentication is your SSH key.** The CLI opens one SSH connection with your normal settings:
  host, user, port, identity files, `ProxyJump` (first hop) and `StrictHostKeyChecking` are
  resolved with `ssh -G`, so `~/.ssh/config` applies (`DOKWALT_SSH_CONFIG` points it at another
  file). It signs in with `ssh-agent` keys first, then your identity files.
- **Host keys are verified against `~/.ssh/known_hosts`.** A changed key is always refused
  ("HOST KEY MISMATCH"). An unknown key is shown with its fingerprint and you are asked, like
  `ssh`; with `StrictHostKeyChecking accept-new` (or `no`) it is accepted without asking, with
  `yes` it is refused. Accepted keys are appended to `known_hosts`.
- **No control-plane port.** For each API connection the CLI runs `dokwalt dial-stdio` in an SSH
  session; it bridges stdin/stdout to the daemon's Unix socket. It works even when sshd has
  `AllowTcpForwarding no` and `AllowStreamLocalForwarding no`.
- **The socket** `/run/dokwalt/dokwalt.sock` is `root:dokwalt`, mode `0660`: only root and members
  of group `dokwalt` can talk to the daemon.
- **The `docker` group is root-equivalent** (`docker run -v /:/host …` can read and write
  anything). `server init` adds your SSH user to `docker` and `dokwalt`. Never add other accounts.

**Why no extra DokWalt password in v0.1?** The admin user is in `docker`, so any secret DokWalt
checked could be bypassed by that same user with one `docker` command: prompts without a real
boundary. What actually protects the server is the SSH key, so protect it:

- a passphrase-protected Ed25519 key, or a hardware-backed agent (Secretive, 1Password, YubiKey);
- limited agent lifetimes, e.g. `ssh-add -t 8h`, so an unattended Mac is not an open door;
- `PasswordAuthentication no` and `PermitRootLogin no` on the server;
- no port 22 forwarded from home; reach a home server over a VPN such as Tailscale.

## Secrets

- **Encrypted at rest.** Config values and config snapshots are stored in SQLite encrypted with
  XChaCha20-Poly1305. The key, 32 random bytes, is `/var/lib/dokwalt/secret.key` (mode `0600`),
  created by the daemon at its first start.
- **Not encrypted:** alert webhook URLs (`alerts:add`) are stored in clear in `dokwalt.db`;
  `dokwalt alerts` masks them in its output. Treat a leaked database as a leaked webhook.
- **What encryption buys you:** a leaked `dokwalt.db` or a copied backup doesn't reveal config
  values without the key. It does not protect against root on the running server, which can
  read the key and every container's environment anyway.
- **Never written to disk in clear.** Rendered compose files under `/var/lib/dokwalt/apps/`
  contain config keys only as pass-through entries (`KEY: null`) and literal `${VAR}`
  references; the values exist only in the environment of the `docker compose` process the
  daemon runs.
- **Masked output.** The daemon masks values before they leave it: `dokwalt config` shows the
  first 2 characters followed by `•••` (values of 4 characters or less fully masked);
  `--reveal` and `--shell` print them in clear, and `config:get <KEY>` prints one value for scripts.
- **Runtime exposure.** Config reaches stateless containers as environment variables (stateful
  ones only get what they reference as `${VAR}`). Anyone in `docker` can read them with
  `docker inspect`: one more reason that group stays yours alone.
- **Exports are plaintext.** `dokwalt apps:export file.json` writes config in clear text (mode
  `0600`, with a warning); without a file it prints to stdout. Move it over SSH, import, delete it.
- **Backups:** store `secret.key` separately from database backups. Without it, config is
  unrecoverable, by design.
- **Nothing secret in the repo.** DokWalt adds no file to your project. Load a local `.env` once
  with `dokwalt config:import .env` and keep `.env` in `.gitignore`.

## Network exposure

| Port | Listener | Exposure |
|---|---|---|
| 22/tcp | sshd | public, or restricted by firewall / VPN |
| 80/tcp, 443/tcp, 443/udp | `dokwalt-caddy` | public |
| anything else | nothing | never published |

- `ports:` in your compose file are **removed** (with a warning). Docker-published ports bypass
  ufw, so this is what keeps a stray `5432:5432` from exposing a database to the internet.
- Any `network_mode` other than `bridge` is rejected; `container_name` is removed.
- Caddy's admin API is a **Unix socket** (`/var/lib/dokwalt/caddy/run/admin.sock`), never TCP
  port 2019. Its access and TLS logs go to another Unix socket read by the daemon
  (`access.sock`) and are only aggregated into metrics, never written to disk.
- **Databases are never public.** `dokwalt db:connect` opens a listener on your Mac and tunnels
  it through SSH with `dokwalt dial-stdio --tcp` to the database container. Client passwords are
  passed through `PGPASSWORD`, `MYSQL_PWD` or `REDISCLI_AUTH`, never on the command line.

Check it yourself on the server:

```bash
sudo ss -tlnup     # expect: sshd on 22, docker-proxy on 80 and 443 (tcp + udp), nothing else
```

## Container isolation

- Each app stage has its own private bridge network, `dw-<app>-<stage>`. Its services (data,
  blue and green projects) reach each other by service name, e.g. `db:5432`.
- There is **no shared proxy network**. The daemon attaches `dokwalt-caddy` to every stage
  network; Caddy is the only container on more than one network. So apps can't reach each
  other at all, databases or web services, and staging can't reach production.
- DokWalt adds no seccomp/AppArmor profile beyond Docker's defaults. Compose security options
  (`privileged`, `cap_add`, `devices`, a Docker socket mount) are kept as written, without a
  warning: each is root-equivalent, so review images that ask for them.
- Absolute host bind mounts are allowed (warning: "host path must exist on the server");
  binds of project files (`./x:/y`) are rejected.
- Recommended in your own files: a non-root `USER` in the Dockerfile, `read_only: true` where
  possible, `cap_drop: [ALL]`, and `mem_limit` so one app cannot starve the others.

## The daemon itself

The daemon runs as root under systemd (it drives Docker and reads `/proc`, `/sys` and cgroups),
with no network listener and the unit's hardening options `NoNewPrivileges=yes`, `PrivateTmp=yes`,
`ProtectSystem=full`, `ProtectKernelTunables=yes` and `LockPersonality=yes` (see `systemctl cat
dokwalt`). It only accepts requests on its socket, i.e. from you over SSH.

## Supply chain

- **Release binaries** are published on GitHub with a SHA-256 `checksums.txt`; there is no
  signature (no cosign) yet. When `server init` or `server upgrade` downloads a server binary,
  it verifies it against `checksums.txt` ("checksum mismatch" aborts).
- **Docker** comes from Docker's official get.docker.com script, never the distro's `docker.io`.
- **Caddy** runs from the official `caddy:2-alpine` image (a floating tag, not a digest). It is
  pulled when the container is first created and recreated only if it isn't running or its
  compose definition changes.
- **Built images** are tagged by content (`dokwalt/<app>-<service>:<12 hex of the image ID>`), so a
  rollback runs exactly the same bytes, as long as the image wasn't pruned.
- **Registry images** (e.g. `postgres:16`) are **not** pinned by digest: they get `pull_policy:
  missing` and are pulled only if absent on the server. Pin a specific tag if that matters.
- **Builds happen on your Mac.** No CI system holds credentials; nothing builds on the server.
  Private registries: `sudo docker login` on the server (the daemon uses root's Docker config).

## No telemetry

DokWalt never phones home and does no update checks; the CLI contacts GitHub only to download
a server binary. The server's outbound traffic is what you configure: Let's Encrypt (Caddy), the
registries you pull from, your alert webhooks (`https://` required), OS/Docker repositories.

## Updates

- `dokwalt server upgrade` re-runs the idempotent bootstrap with the CLI's version: it replaces
  the binary and restarts the daemon. Apps keep serving; Caddy is recreated only if its compose
  definition changed (about a 1 s blip). It takes no database backup: make your own first.
- `dokwalt doctor` warns (check *Versions*) when the CLI and the daemon builds differ.
- OS and Docker security fixes: `unattended-upgrades` with a nightly reboot window (see
  `dokwalt docs raspberry-pi` or `dokwalt docs vps`).

## Checklist

1. SSH works with keys only; password and root logins are refused.
2. Only your admin user is in `dokwalt` and `docker` (`getent group docker dokwalt`).
3. `sudo ss -tlnup` shows only 22, 80 and 443.
4. `secret.key` and backups are stored off the server, apart from each other.
5. No `.env` or `apps:export` file is committed or lying around.

See also: `dokwalt docs config`, `dokwalt docs databases`, `dokwalt docs raspberry-pi`,
`dokwalt docs vps`.
