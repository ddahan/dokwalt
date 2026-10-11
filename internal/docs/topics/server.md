# Setting up a server

*From a fresh Debian or Ubuntu machine, a VPS or your own, to a hardened, reboot-proof DokWalt server.*

DokWalt runs on any 64-bit Linux machine with systemd, **amd64 or arm64**: a VPS, a mini PC, a
home server or a single-board computer. Each step is tagged **[by hand]** or **[server init]**
(done for you by `dokwalt server init`). Commands run on the server unless the block starts with
`# on your computer`. Where a rented server and a machine at home differ, the step says so.

## 1. Choose the machine [by hand]

| RAM | Good for | Notes |
|---|---|---|
| 1 GB | 4–6 small sites with databases | Add swap (step 4) |
| 2 GB | ~10 small sites | Comfortable default |
| 4 GB | 10–20 sites | Room for several Postgres |
| 8 GB+ | 20–30+ sites | Heavier stacks: JVM, Elasticsearch |

DokWalt's overhead is ~73 MiB (daemon ~27 MiB RSS, ~9 MiB of it heap; Caddy ~46 MiB). Both disable
transparent huge pages for themselves, so leave the system THP setting alone. A small dynamic site
with its own Postgres needs 150–400 MB, a static site 10–30 MB: sites run out of RAM long before
CPU. During a deploy the old and new versions of an app's stateless services run side by side
(about 10–15 s with the default 10 s drain), so keep headroom for your largest app twice.

**Disk:** 20 GB minimum. Per stage, the images of the current release and the last 5 releases are
kept (older ones pruned after each deploy).

**Your own hardware:**
- **An SSD, not an SD card or a USB stick.** Docker layers, SQLite, logs and the journal write
  constantly. Flash cards have weak wear-levelling and no power-loss protection; a worn card fails
  silently (read-only or corrupt filesystem), the classic "broken after reboot".
- **Wired Ethernet** rather than Wi-Fi, and a power supply rated for the board. A small UPS rides
  out short power cuts, but DokWalt doesn't need one (step 4, *Power cuts*).

## 2. Operating system [by hand]

Use **Debian 12 / 13** or **Ubuntu 24.04 LTS** (or newer LTS), 64-bit, as a minimal server image:
no desktop. Debian-based distributions built for a specific board work too. 32-bit systems are
refused. On a VPS, pick the provider's image. On your own machine, install the server image with
the OpenSSH server enabled. Then bring it up to date:

```bash
sudo apt update && sudo apt full-upgrade -y && sudo reboot
```

## 3. Your user and SSH access [by hand]

Create a key on your computer if you have none (`ssh-keygen -t ed25519`). Log in with that key as a
normal user with `sudo`, never as root:

- **VPS:** add your public key when creating the server. Most providers inject it with
  cloud-init into a default user: `ubuntu` on Ubuntu, `debian` (or `admin`) on Debian images.
- **Root-only images:** create your own user once, then never log in as root again:

```bash
# as root, once
adduser deploy && usermod -aG sudo deploy
install -d -m 700 -o deploy -g deploy /home/deploy/.ssh
install -m 600 -o deploy -g deploy ~/.ssh/authorized_keys /home/deploy/.ssh/
```

- **Your own machine:** create the user in the installer, then copy your key:
  `ssh-copy-id deploy@192.168.1.50` from your computer.

Cloud-init users often have passwordless sudo; `server init` then simply won't prompt. Set a
password anyway (`sudo passwd deploy`) so you can use the provider's web console or a local screen.

Give the server an alias in `~/.ssh/config` on your computer (DokWalt resolves it with `ssh -G`;
`DOKWALT_SSH_CONFIG` selects another file), then `ssh prod` once to record the host key
(otherwise DokWalt shows the fingerprint and asks):

```text
Host prod
  HostName 203.0.113.10
  User deploy
```

## 4. System configuration [by hand]

**Memory cgroup.** Needed for memory limits, `docker stats` and per-service memory metrics. It is
on by default on Debian 12+ and Ubuntu 24.04 (cgroup v2), but some kernels built for ARM boards
disable it; `server init` warns and `dokwalt doctor` (*Memory cgroup*) flags it when off:

```bash
grep memory /sys/fs/cgroup/cgroup.controllers   # must print a line
```

If it prints nothing, add `cgroup_enable=memory cgroup_memory=1` to the kernel command line (the
file depends on the bootloader; it is a single line) and reboot.

**Swap.** On a 1–2 GB machine, swap turns an out-of-memory kill during a deploy into a short
slowdown. A plain swap file is fine on a VPS or an SSD:

```bash
sudo fallocate -l 1G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
echo 'vm.swappiness=10' | sudo tee /etc/sysctl.d/99-swappiness.conf && sudo sysctl --system
```

On flash storage you'd rather not wear, use zram instead: compressed swap in RAM, zero disk writes.

```bash
sudo apt install -y zram-tools && printf 'ALGO=zstd\nPERCENT=25\n' | sudo tee /etc/default/zramswap
sudo systemctl restart zramswap && swapon --show
```

**Bounded logs.** Cap the journal: `persistent` keeps the last boot's logs (`journalctl -b -1`).
Container logs are capped by DokWalt (`local` driver, 10 MB × 3 each).

```bash
sudo mkdir -p /etc/systemd/journald.conf.d
printf '[Journal]\nStorage=persistent\nSystemMaxUse=100M\nRuntimeMaxUse=50M\n' \
  | sudo tee /etc/systemd/journald.conf.d/dokwalt.conf
sudo systemctl restart systemd-journald
```

**Time sync.** A wrong clock breaks TLS and Let's Encrypt. `timedatectl` must say `System clock
synchronized: yes` (`doctor` checks it); if NTP is inactive, `sudo timedatectl set-ntp true`.

**Hardware watchdog** (most physical machines; usually absent in VMs). If `wdctl` finds one, let
systemd feed it: a frozen kernel then resets the machine by itself. DokWalt's own unit adds
`WatchdogSec=60` either way: a daemon stuck for 10 minutes gets restarted.

```bash
sudo mkdir -p /etc/systemd/system.conf.d
printf '[Manager]\nRuntimeWatchdogSec=15s\nRebootWatchdogSec=2min\n' \
  | sudo tee /etc/systemd/system.conf.d/watchdog.conf && sudo systemctl daemon-reexec && wdctl
```

**Power cuts** (your own machine). Set the firmware's *restore on AC power loss* option to
*power on*, so the machine boots by itself when power returns. Nothing else to enable: ext4 on an
SSD, SQLite in WAL mode, restart policies and the reconciler do the rest.

## 5. Networking [by hand]

Every domain needs an A record (and optionally AAAA) pointing at the server, and **ports 80/tcp**
(Let's Encrypt HTTP-01 and the HTTPS redirect), **443/tcp** (HTTPS) and optionally **443/udp**
(HTTP/3) reachable from the internet. Don't expose SSH more than you need.

### On a VPS

- **Public IP, no NAT:** no port forwarding, no dynamic DNS. Point each domain's A record at the
  server's IPv4 (`curl -4 -s https://ifconfig.me` to confirm it).
- **Provider firewall / security groups:** allow `22/tcp` (ideally only from your IP), `80/tcp`,
  `443/tcp`, `443/udp`. It sits outside the VM, so Docker cannot bypass it: a good second layer
  on top of ufw (step 6).

### At home, behind a router

- **Fixed LAN address:** a DHCP reservation on the router (the MAC address is in `ip link`), e.g.
  192.168.1.50. Better than a static IP on the machine: the router stays the single source of truth.
- **Port forwarding** of 80/tcp, 443/tcp and 443/udp to that address. Don't forward 22; use
  Tailscale for remote SSH.
- **Changing public IP:** the router's DDNS client, or `ddclient` with your DNS provider's API
  (Cloudflare is supported) updating the A/AAAA records. Keep TTLs low (300 s).
- **CGNAT:** compare the router's WAN IP with `curl -4 -s https://ifconfig.me`. If they differ, or
  the WAN IP is in 100.64.0.0/10, forwarding cannot work. Ask the ISP for a public IPv4, or:
  - **Cloudflare Tunnel** (`cloudflared`, free): Cloudflare terminates visitor TLS at its edge.
    DokWalt only does HTTP-01 (no DNS-01), so use two ingress rules per host: path
    `^/\.well-known/acme-challenge/` → `http://localhost:80`, everything else →
    `https://localhost:443` with `originServerName: <host>`. The `domains:add` check may report
    that the domain isn't reachable (DNS points at Cloudflare): expected, the domain is added anyway.
  - **Tailscale Funnel** serves only `*.ts.net` names on 443 and terminates TLS itself (no port
    80, no custom domains): demos only. Plain Tailscale suits private admin access (SSH, DokWalt CLI).

### IPv6

Publish AAAA records only if IPv6 really works end to end: Let's Encrypt prefers IPv6, so a broken
AAAA record means no certificate. On a VPS, check it is configured and allowed in the provider
firewall. At home, use the stable address from `ip -6 addr show scope global`, allow inbound
80/443 to it in the router's IPv6 firewall, and let DDNS follow prefix changes. Docker needs no
IPv6 networking: Caddy listens on both.

## 6. Hardening [by hand]

**SSH keys only, no root; firewall: SSH (throttled by `ufw limit`) and web.** Test a new login
before closing your session. Name the drop-in `10-…` so it wins: sshd keeps the first value it
reads, and Ubuntu cloud images ship `50-cloud-init.conf`, which may enable passwords.

```bash
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\nMaxAuthTries 3\n' \
  | sudo tee /etc/ssh/sshd_config.d/10-dokwalt.conf && sudo sshd -t && sudo systemctl reload ssh
sudo apt install -y ufw && sudo ufw default deny incoming && sudo ufw default allow outgoing
sudo ufw limit 22/tcp; for p in 80/tcp 443/tcp 443/udp; do sudo ufw allow $p; done; sudo ufw enable
```

**The Docker gotcha:** container-published ports go through Docker's own iptables rules and
bypass ufw, so `ports: ["5432:5432"]` would be public despite `ufw deny`. DokWalt avoids it by
design: it strips `ports:` from your compose file (with a warning), only `dokwalt-caddy`
publishes 80/443, and apps stay on private `dw-<app>-<stage>` networks (Caddy joins each; apps
can't reach each other). `sudo ss -tlnp` should list only sshd on 22 and docker-proxy on 80/443.

**Automatic security updates**, rebooting at 04:00 when needed. Safe with DokWalt: reboots are
what it is built and tested for, and a site-down alert fires if a site doesn't come back.
`unattended-upgrades` is preinstalled on Ubuntu and most Debian cloud images.

```bash
sudo apt install -y unattended-upgrades && sudo dpkg-reconfigure -plow unattended-upgrades
printf 'Unattended-Upgrade::Automatic-Reboot "true";\nUnattended-Upgrade::Automatic-Reboot-Time "04:00";\n' \
  | sudo tee /etc/apt/apt.conf.d/52local
```

**fail2ban?** 40–60 MB of Python against password guessing that key-only SSH makes moot; `ufw
limit`, `MaxAuthTries` and OpenSSH ≥ 9.8's `PerSourcePenalties` suffice. **Disable what you don't
use** to save RAM: services such as `bluetooth`, `avahi-daemon`, `cups` or `ModemManager` where
present, and `snapd` on Ubuntu if you don't use snaps (`sudo apt purge snapd`).

**DokWalt's attack surface:** no network port, only `/run/dokwalt/dokwalt.sock` (root:dokwalt,
0660) reached through your SSH login. `dokwalt` and `docker` members are root-equivalent: admin only.

## 7. Install DokWalt [server init]

```bash
# on your computer
dokwalt server init deploy@203.0.113.10 --name prod --email you@example.com
dokwalt doctor
```

`server init` detects the architecture (amd64 or arm64) and uploads the matching binary
(`--binary`, `dokwalt_linux_<arch>` next to the CLI or in `dist/`, else the release download
checked against `checksums.txt`). It then runs `sudo … server bootstrap` in a terminal session
(you type your password once; none with passwordless sudo or root), which:

- installs Docker with the official get.docker.com script if missing (enabled at boot), and
  `docker-compose-plugin` via apt if missing. A Docker you installed yourself is kept;
- merges `live-restore: true` (plus `log-driver: local` if none is set) into
  `/etc/docker/daemon.json`, restarting Docker only if that file changed;
- warns if the memory cgroup is off (step 4);
- creates group `dokwalt` and adds you to `dokwalt` and `docker`;
- installs `/usr/local/bin/dokwalt` and `/var/lib/dokwalt`, writes, enables and starts
  `dokwalt.service`, waits for its socket and stores the `--email` (Let's Encrypt notices).

The daemon then creates `secret.key` and starts Caddy. Re-running `server init`, or `dokwalt
server upgrade`, is safe at any time (idempotent).

Pass `--name`: without it an IP target is saved as `default`. The first server becomes the
current one; with several use `--server prod` or `dokwalt server use prod`. Any `~/.ssh/config`
alias or `user@host:port` works as a target. Private registries: `sudo docker login` on the
server; the unit sets `HOME=/root`, so deploys use root's Docker credentials.

### arm64 servers

Images are built on your computer for the server's architecture: natively on an arm64 computer
(Apple Silicon, arm64 Linux), under emulation on an amd64 one (slower; Docker Desktop includes
it, with Docker Engine on Linux install it once with
`docker run --privileged --rm tonistiigi/binfmt --install arm64`). Images pulled from a registry
must publish `linux/arm64` too. Check before deploying:

```bash
# on your computer
docker manifest inspect some/image:tag | grep -c arm64      # 0 = no arm64 variant
```

No arm64 variant? Use another maintained image, or give the service a `build:` pointing at the
upstream source: DokWalt builds it on your computer for `linux/arm64`. Don't run amd64 images under
QEMU on the server (5–20× slower). Wrong architecture shows as `exec format error`.

## 8. Health and alerts

`dokwalt doctor`, `dokwalt top` and the dashboard show CPU, memory, disk and load. Where the
machine exposes a thermal sensor (most physical machines, rarely VMs), they show the temperature
too, and the temperature alert fires above 80 °C by default; boards whose firmware reports CPU
throttling or under-voltage get a throttling alert as well. Without a sensor these lines and
alerts are simply omitted. Disk and memory alerts (default 90 %) matter everywhere:

```bash
dokwalt alerts:add discord <webhook-url>
dokwalt alerts:set disk=85 memory=85 temperature=75
```

See `dokwalt docs alerts` and `dokwalt docs monitoring`.

## 9. Backups and restore

**Databases and DokWalt's state:** `dokwalt backup:setup` sends them to Cloudflare R2 (or any S3
storage) every night (`dokwalt docs backups`), off the server: a dead disk doesn't take the data
with it. Keep a copy of `/var/lib/dokwalt/secret.key` apart from the bucket.

**Whole machine:** on a VPS, provider snapshots are the easiest full backup: schedule them daily or
weekly. A snapshot of a running server is crash-consistent: databases recover as after a power cut.

**By hand:** back up `/var/lib/dokwalt/dokwalt.db` (apps, releases, domains, encrypted config),
`secret.key` (without it config cannot be decrypted), the Docker volume `dokwalt-caddy-data`
(certificates and ACME account: no re-issuance, no rate limits after a restore) and the `dw-*`
volumes (databases, uploads). Config values exist nowhere else on disk; `caddy/config/caddy.json`
is regenerated.

```bash
sudo apt install -y sqlite3; B=/srv/backup/$(date +%F); sudo mkdir -p $B
sudo sqlite3 /var/lib/dokwalt/dokwalt.db ".backup $B/dokwalt.db"
sudo cp /var/lib/dokwalt/secret.key $B/
for v in dokwalt-caddy-data $(docker volume ls -q --filter name=dw-); do
  docker run --rm -v $v:/v:ro -v $B:/b alpine tar czf /b/$v.tgz -C /v .
done
```

A tar of a live database may be inconsistent: also `pg_dump` the URL printed by `dokwalt db:connect
-a blog --service db --tunnel-only`. Copy backups off the server; treat `secret.key` like a password.

**Restore onto a fresh server:** steps 1–7 (`ssh-keygen -R <host>` on your computer), backup in `$B`, then:

```bash
sudo systemctl stop dokwalt && docker stop dokwalt-caddy
sudo rm -f /var/lib/dokwalt/dokwalt.db-wal /var/lib/dokwalt/dokwalt.db-shm
sudo cp $B/dokwalt.db $B/secret.key /var/lib/dokwalt/ && sudo chmod 600 /var/lib/dokwalt/secret.key
for f in $B/*.tgz; do v=$(basename $f .tgz)
  docker run --rm -v $v:/v -v $B:/b alpine tar xzf /b/$v.tgz -C /v
done
sudo systemctl start dokwalt    # reconciler: Caddy, networks, registry-image stacks
```

Built images aren't in the backup: until you `dokwalt deploy` once from each app folder, those
apps stay down, the reconciler alerts "Self-healing failed" and `doctor` marks them ✗ (registry
images are pulled again). Moving servers without the state DB: `dokwalt apps:export blog.json`
(old server), `apps:import blog.json` (new one), restore volumes, `deploy`; release history is lost.

## 10. Final checklist

1. `dokwalt doctor` is all ✓ and `dokwalt alerts:test` reaches Discord/Slack.
2. The site opens over HTTPS from outside: `curl -I https://blog.example.com` from your computer, and for
   a server at home, on your phone **on mobile data** (home Wi-Fi can be fooled by hairpin NAT).
3. **Reboot test**, from your computer: `ssh -t prod sudo reboot`, then time
   `until curl -sfo /dev/null https://blog.example.com; do sleep 2; done`. Must be < 2 min.
4. **Power-cut test:** pull the plug while a page loads (on a VPS: a hard reset from the provider
   panel), wait 10 s, power back on, same loop, then `dokwalt doctor`, `dokwalt releases` (a
   deploy cut mid-way shows `failed`) and on the server
   `sudo sqlite3 /var/lib/dokwalt/dokwalt.db 'PRAGMA integrity_check'` (must print `ok`).
5. Optional watchdog test, if you set one up (kernel crash; the machine must come back):
   `echo c | sudo tee /proc/sysrq-trigger`.

See also: `dokwalt docs getting-started`, `dokwalt docs reboot-recovery`, `dokwalt docs security`,
`dokwalt docs troubleshooting`.
