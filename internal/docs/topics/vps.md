# VPS

*Running DokWalt on a Debian or Ubuntu VPS: only what differs from the Raspberry Pi guide.*

Supported: **Debian 12 / 13** and **Ubuntu 24.04 LTS** (or newer LTS), amd64 or arm64. DokWalt detects the
architecture during `server init` and builds your images for it on the Mac. Everything in
`dokwalt docs raspberry-pi` about SSH, ufw and updates applies; this page covers the differences.

## Sizing

| RAM | Good for | Notes |
|---|---|---|
| 1 GB | 4–6 small sites with DBs | Add a swap file (below) |
| 2 GB | ~10 small sites | Comfortable default |
| 4 GB+ | 20+ sites, heavier stacks | JVM, Elasticsearch, several Postgres |

DokWalt's overhead is ~73 MiB (daemon ~27 MiB RSS, Caddy ~46 MiB); both disable transparent huge
pages for themselves, so the host THP setting doesn't matter. During a deploy the old and new
versions of an app's stateless services run side by side (about 10–15 s with the default 10 s
drain), so keep headroom for your largest app twice. Disk: 20 GB minimum; per stage, the images
of the current release and the last 5 releases are kept (older ones pruned after each deploy).

**Swap on a 1 GB box.** A VPS disk is not yours to wear out, so a plain swap file is fine: it
turns an out-of-memory kill during a deploy into a short slowdown.

```bash
sudo fallocate -l 1G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
echo 'vm.swappiness=10' | sudo tee /etc/sysctl.d/99-swappiness.conf && sudo sysctl --system
```

## Creating the server

- **Add your SSH public key at creation.** Most providers inject it with cloud-init into a
  default user: `ubuntu` on Ubuntu, `debian` (or `admin`) on Debian images.
- **Root-only images:** create your own user once, then never log in as root again.

```bash
# as root, once
adduser dd && usermod -aG sudo dd
install -d -m 700 -o dd -g dd /home/dd/.ssh
install -m 600 -o dd -g dd ~/.ssh/authorized_keys /home/dd/.ssh/
```

Cloud-init users often have passwordless sudo; `server init` then simply won't prompt.
Set a password anyway (`sudo passwd dd`) so you can use the provider's web console.

## Networking: simpler than at home

- **Public IP, no NAT:** no port forwarding, no dynamic DNS, no CGNAT. Point the A record of
  each domain at the server's IPv4 (`curl -4 -s https://ifconfig.me` to confirm it).
- **IPv6:** most providers assign a /64. Add AAAA records only if IPv6 is configured and allowed
  in the provider firewall; Let's Encrypt prefers IPv6, so a broken AAAA means no certificate.
- **Provider firewall / security groups:** allow `22/tcp` (ideally only from your IP),
  `80/tcp`, `443/tcp`, `443/udp`. It sits outside the VM, so Docker cannot bypass it: a good
  second layer on top of ufw.

## Hardening: same as the Pi

SSH keys only and no root login. Name the drop-in `10-…` so it wins: sshd keeps the first value
it reads, and Ubuntu cloud images ship `50-cloud-init.conf`, which may enable passwords.

```bash
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\n' \
  | sudo tee /etc/ssh/sshd_config.d/10-dokwalt.conf
sudo sshd -t && sudo systemctl reload ssh
sudo apt install -y ufw && sudo ufw default deny incoming && sudo ufw default allow outgoing
sudo ufw limit 22/tcp; for p in 80/tcp 443/tcp 443/udp; do sudo ufw allow $p; done; sudo ufw enable
```

The Docker-bypasses-ufw gotcha is identical, and DokWalt avoids it the same way: `ports:` are
stripped, only `dokwalt-caddy` publishes 80/443, and each app stage has its own private network
(`dw-<app>-<stage>`, with Caddy attached), so apps can't reach each other either.

**Automatic security updates:** `unattended-upgrades` is preinstalled on Ubuntu and most Debian
cloud images. Enable controlled reboots, which DokWalt is built to survive:

```bash
sudo apt install -y unattended-upgrades && sudo dpkg-reconfigure -plow unattended-upgrades
printf 'Unattended-Upgrade::Automatic-Reboot "true";\nUnattended-Upgrade::Automatic-Reboot-Time "04:00";\n' \
  | sudo tee /etc/apt/apt.conf.d/52local
```

**Trim a 1 GB Ubuntu box** (optional): if you don't use snaps, `sudo apt purge snapd` frees RAM.

## What doesn't apply on a VPS

- **Temperature and throttling:** no `vcgencmd` and usually no thermal sensor, so `doctor`, `top`
  and the dashboard simply omit them and those alerts never fire. Disk and memory alerts (default
  90 %) matter more: `dokwalt alerts:set disk=85 memory=85`.
- **SD cards, NVMe boot, EEPROM, RTC, zram tuning:** none of it.
- **Memory cgroup:** enabled by default on Debian 12+ and Ubuntu 24.04 (cgroup v2); `doctor`
  still checks it.
- **Time sync:** `systemd-timesyncd` or `chrony` is preinstalled; confirm with `timedatectl`.
- **Hardware watchdog:** usually absent in VMs. DokWalt's own systemd watchdog still restarts a
  hung daemon.

## Install DokWalt

```bash
# on the Mac
dokwalt server init dd@203.0.113.10 --name prod --email you@example.com
dokwalt doctor
```

`server init` detects the architecture, uploads the matching binary and runs the bootstrap with
`sudo` in a terminal session (you type your password once; none with passwordless sudo or root).
The bootstrap installs Docker with the official get.docker.com script if missing (and the
`docker-compose-plugin` via apt if missing), merges `live-restore: true` and, if no log driver is
set, `log-driver: local` into `/etc/docker/daemon.json`, creates group `dokwalt` and adds you to
`dokwalt` and `docker`, installs `/usr/local/bin/dokwalt`, writes and starts `dokwalt.service`,
and stores the `--email`. The daemon then creates `secret.key` and starts Caddy. It is idempotent:
re-run it, or `dokwalt server upgrade`, at any time.

Pass `--name`: without it an IP target is saved as `default`. The first server becomes the
current one; with several use `--server prod` or `dokwalt server use prod`. Any `~/.ssh/config`
alias or `user@host:port` works as a target. Private registries: `sudo docker login` on the
server; the unit sets `HOME=/root`, so deploys use root's Docker credentials.

## Backups

- **Provider snapshots** are the easiest full-machine backup: schedule them daily or weekly.
  A snapshot of a running server is crash-consistent: databases recover as after a power cut.
- Add **logical database dumps** through `dokwalt db:connect --tunnel-only` and keep a copy of
  `/var/lib/dokwalt/secret.key` off the server. File-level backups are the same as on a Pi
  (`dokwalt docs raspberry-pi`): `dokwalt.db`, `secret.key`, the Docker volume
  `dokwalt-caddy-data` (certificates) and the `dw-*` volumes.
- After a file-level restore, built images are missing: run `dokwalt deploy` once per app folder
  (until then the reconciler alerts "Self-healing failed" for those apps).
- Moving providers: `dokwalt apps:export` / `dokwalt apps:import`, restore volumes, `deploy`.

## Final checklist

1. `dokwalt doctor` is all ✓; `dokwalt alerts:test` reaches Discord/Slack.
2. `curl -I https://blog.example.com` from your Mac returns `200` (or your redirect).
3. `ssh -t dd@203.0.113.10 sudo reboot`: every site serves HTTPS again within about a minute.
4. Hard reset from the provider panel (the VPS equivalent of pulling the plug), then
   `dokwalt doctor` and `dokwalt releases`.

See also: `dokwalt docs raspberry-pi`, `dokwalt docs security`, `dokwalt docs reboot-recovery`,
`dokwalt docs getting-started`.
