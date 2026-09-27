# Raspberry Pi 5

*From a bare Raspberry Pi 5 to a hardened, reboot-proof DokWalt server, step by step.*

Each step is tagged **[by hand]** or **[server init]** (done for you by `dokwalt server init`).
Commands run on the Pi unless the block starts with `# on the Mac`.

## 1. Hardware [by hand]

| Part | Choose | Why |
|---|---|---|
| RAM | 8 GB (4 GB is fine to start) | Sites run out of RAM long before CPU |
| Power | Official 27 W USB-C PSU (5.1 V/5 A) | Weaker PSUs: under-voltage, throttling, NVMe resets |
| Cooling | Official Active Cooler | Otherwise throttling at 80–85 °C under load |
| Storage | NVMe SSD on an M.2 HAT+, 256 GB+ | See below |
| Extras | RTC battery; UPS HAT optional | Correct clock before NTP; ride out short cuts |

Sizing: DokWalt's overhead is ~73 MiB (daemon ~27 MiB RSS, ~9 MiB of it heap; Caddy ~46 MiB). Both
disable transparent huge pages for themselves, so leave the system THP setting alone. A small
dynamic site with its own Postgres needs 150–400 MB, a static site 10–30 MB: **4 GB** ≈ 8–12
dynamic sites, **8 GB** ≈ 20–30, with headroom for heavy stacks (JVM, Elasticsearch).

**Why NVMe, not SD:** Docker layers, SQLite, logs and the journal write constantly. SD cards have
weak wear-levelling and no power-loss protection; a worn card fails silently (read-only or corrupt
filesystem), the classic "broken after reboot". NVMe is also far faster for images and databases.

**Boot from NVMe.** Easiest: put the SSD in a USB M.2 enclosure, flash it from the Mac (step 3),
then mount it on the HAT. Or boot once from SD and clone with `sudo rpi-clone nvme0n1` (check
`lsblk` first). Update the bootloader (`sudo rpi-eeprom-update -a && sudo reboot`), then run
`sudo rpi-eeprom-config --edit` (or raspi-config → Advanced Options → Boot Order) and set:
```ini
BOOT_ORDER=0xf416   # right to left: NVMe (6), SD (1), USB (4), retry (f)
PCIE_PROBE=1        # only for non-HAT+ adapters (no ID EEPROM on the board)
```
Power off, remove the SD card, boot; `findmnt /` must show `/dev/nvme0n1p2`. RTC battery: enable
charging (rechargeable cells only) with `dtparam=rtc_bbat_vchg=3000000` in `/boot/firmware/config.txt`.

## 2. Operating system [by hand]

| | Pi OS Lite 64-bit | Ubuntu Server LTS arm64 | DietPi |
|---|---|---|---|
| Pi 5 kernel/firmware | First-party, fixes first | Supported, lags | Pi kernel |
| `vcgencmd`, EEPROM tools | Built in | Extra packages | Built in |
| Idle footprint | Small | Larger (snapd, cloud-init) | Smallest |
| Support | Follows Debian | 5 years per LTS | Small team, custom scripts |

**Use Raspberry Pi OS Lite (64-bit):** Pi 5 fixes land there first, and it is plain Debian, which
Docker supports officially. Pick Ubuntu only to match an Ubuntu VPS. 64-bit is mandatory.

## 3. Flash with Raspberry Pi Imager [by hand]

Device *Raspberry Pi 5* → OS *Raspberry Pi OS (other) → Lite (64-bit)* → the SSD. Customise:
- **Hostname** `pi`; **username** anything but `pi` (e.g. `dd`), strong password (for `sudo`).
- **SSH** enabled, *public-key authentication only*, paste `~/.ssh/id_ed25519.pub`
  (`ssh-keygen -t ed25519` if you have none). Password logins never work, from day one.
- **Locale/timezone** yours (local times in logs); **Wi-Fi** empty on Ethernet (wired is more reliable).

## 4. First boot and updates [by hand]

Create the DHCP reservation (step 7), add `Host pi.home` / `HostName 192.168.1.50` / `User dd` to
`~/.ssh/config` (DokWalt resolves it with `ssh -G`; `DOKWALT_SSH_CONFIG` selects another file), and
`ssh pi.home` once to record the host key (else DokWalt shows the fingerprint and asks). Then:
```bash
sudo apt update && sudo apt full-upgrade -y   # full-upgrade: kernel/firmware may add/drop packages
sudo rpi-eeprom-update -a && sudo reboot      # bootloader fixes (NVMe, power, thermal)
```

## 5. System configuration [by hand]

**Memory cgroup.** Needed for memory limits, `docker stats` and per-service memory metrics; `server
init` warns and `dokwalt doctor` (*Memory cgroup*) flags it when off. Some Pi OS kernels disable it:
```bash
grep memory /sys/fs/cgroup/cgroup.controllers   # must print a line; if not (cmdline.txt is ONE line):
sudo sed -i '1 s/$/ cgroup_enable=memory cgroup_memory=1/' /boot/firmware/cmdline.txt && sudo reboot
```

**zram, not a swap file.** `dphys-swapfile` swaps to disk; zram is compressed swap in RAM that
absorbs spikes with zero disk writes. Skip if `swapon --show` already lists only `/dev/zram0`.
```bash
sudo systemctl disable --now dphys-swapfile
sudo apt install -y zram-tools && printf 'ALGO=zstd\nPERCENT=25\n' | sudo tee /etc/default/zramswap
sudo systemctl restart zramswap && swapon --show
```

**Fewer disk writes.** Cap the journal: `persistent` keeps the last boot's logs (`journalctl -b -1`),
`volatile` (RAM only) suits SD. Container logs are capped by DokWalt (`local`, 10 MB × 3 each).
```bash
sudo mkdir -p /etc/systemd/journald.conf.d
printf '[Journal]\nStorage=persistent\nSystemMaxUse=100M\nRuntimeMaxUse=50M\n' \
  | sudo tee /etc/systemd/journald.conf.d/dokwalt.conf
sudo systemctl restart systemd-journald
```

**Time sync.** A wrong clock breaks TLS and Let's Encrypt; `timedatectl` must say `System clock
synchronized: yes` (`doctor` checks it); if NTP is inactive, `sudo timedatectl set-ntp true`.

**Hardware watchdog.** systemd feeds the SoC watchdog (15 s max); if the kernel freezes, the Pi
resets itself. DokWalt's unit adds `WatchdogSec=60`: a main loop stuck 10 min gets it restarted.
```bash
sudo mkdir -p /etc/systemd/system.conf.d
printf '[Manager]\nRuntimeWatchdogSec=15s\nRebootWatchdogSec=2min\n' \
  | sudo tee /etc/systemd/system.conf.d/watchdog.conf && sudo systemctl daemon-reexec && wdctl
```

**Power cuts.** Nothing to enable: ext4 on an SSD, SQLite WAL, restart policies and the reconciler.
The Pi 5 boots when power returns; keep the EEPROM default `POWER_OFF_ON_HALT=0`.

## 6. Docker on ARM [server init]

`server init` installs Docker with the official get.docker.com script if missing (enabled at boot),
`docker-compose-plugin` via apt if missing, and merges `live-restore: true` (plus `log-driver:
local` if none is set) into `/etc/docker/daemon.json`, restarting Docker only if that file changed.
A Docker you installed yourself (Docker's *Install on Debian* guide) is kept.

**arm64 images.** Every registry image must publish `linux/arm64`. Check before deploying:
```bash
# on the Mac
docker manifest inspect some/image:tag | grep -c arm64      # 0 = no arm64 variant
```
No arm64 variant? Use another maintained image, or give the service a `build:` pointing at the
upstream source: DokWalt builds it on your Mac for `linux/arm64`, natively on Apple Silicon. Don't
run amd64 images under QEMU on the Pi (5–20× slower). Wrong arch shows as `exec format error`.

## 7. Home networking [by hand]

**Fixed LAN address:** a DHCP reservation on the router (MAC from `ip link show eth0` →
192.168.1.50). Better than a static IP on the Pi: the router stays the single source of truth.
**Port forwarding** to it: `80/tcp` (Let's Encrypt HTTP-01, redirect to HTTPS), `443/tcp`
(HTTPS), `443/udp` (HTTP/3, optional). Don't forward 22; use Tailscale for remote SSH.
**Changing public IP:** the router's DDNS client, or `ddclient` with your DNS provider's API
(Cloudflare is supported) updating the A/AAAA records. Keep TTLs low (300 s).

**CGNAT:** compare the router's WAN IP with `curl -4 -s https://ifconfig.me`. If they differ, or
the WAN IP is in 100.64.0.0/10, forwarding cannot work. Ask the ISP for a public IPv4, or:
- **Cloudflare Tunnel** (`cloudflared`, free): Cloudflare terminates visitor TLS at its edge.
  DokWalt only does HTTP-01 (no DNS-01), so use two ingress rules per host: path
  `^/\.well-known/acme-challenge/` → `http://localhost:80`, everything else → `https://localhost:443`
  with `originServerName: <host>`. The `domains:add` check may report that the domain isn't
  reachable (DNS points at Cloudflare): expected, the domain is added anyway.
- **Tailscale Funnel** serves only `*.ts.net` names on 443 and terminates TLS itself (no port 80,
  no custom domains): demos only. Plain Tailscale suits private admin access (SSH, DokWalt CLI).

**IPv6:** publish AAAA only if it works (Let's Encrypt prefers IPv6; broken AAAA = no certificate).
Use the stable address from `ip -6 addr show eth0 scope global`, allow inbound 80/443 to it in the
router's IPv6 firewall, let DDNS follow prefix changes. Docker needs no IPv6: Caddy listens on both.

## 8. Hardening [by hand]

**SSH keys only, no root; firewall: SSH (throttled by `ufw limit`) and web.** Test a new login first.
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
```bash
sudo apt install -y unattended-upgrades && sudo dpkg-reconfigure -plow unattended-upgrades
sudo tee /etc/apt/apt.conf.d/52local <<'EOF'    # Pi origin = kernel + firmware updates too
Unattended-Upgrade::Origins-Pattern { "origin=Raspberry Pi Foundation,codename=${distro_codename},label=Raspberry Pi Foundation"; };
Unattended-Upgrade::Automatic-Reboot "true";
Unattended-Upgrade::Automatic-Reboot-Time "04:00";
EOF
```

**fail2ban?** 40–60 MB of Python against password guessing that key-only SSH makes moot; `ufw
limit`, `MaxAuthTries` and OpenSSH ≥ 9.8's `PerSourcePenalties` suffice. **Disable what you don't
use** (less RAM, radios off); without avahi, `pi.local` stops resolving (use the IP or ssh alias):
```bash
for s in bluetooth avahi-daemon cups ModemManager; do sudo systemctl disable --now $s 2>/dev/null; done
printf '[all]\ndtoverlay=disable-wifi\ndtoverlay=disable-bt\n' | sudo tee -a /boot/firmware/config.txt
```

**DokWalt's attack surface:** no network port, only `/run/dokwalt/dokwalt.sock` (root:dokwalt, 0660)
reached through your SSH login. `dokwalt` and `docker` members are root-equivalent: admin only.

## 9. Install DokWalt [server init]

On the Mac: `dokwalt server init dd@pi.home --name pi --email you@example.com`, then `dokwalt doctor`.
It detects arm64, uploads the binary (`--binary`, `dokwalt_linux_arm64` next to the CLI or in `dist/`,
else the release download checked against `checksums.txt`) and runs `sudo … server bootstrap` in a
terminal (type your sudo password): step 6, the cgroup warning, group `dokwalt`, you in `dokwalt`
and `docker`, `/usr/local/bin/dokwalt`, `/var/lib/dokwalt`, `dokwalt.service` enabled and restarted,
wait for the socket, store `--email`. The daemon then creates `secret.key` and starts Caddy.
Re-running init or `dokwalt server upgrade` is safe (idempotent). `pi` becomes the current server
if it's your first. Private registries: `sudo docker login` on the Pi (the unit sets `HOME=/root`).

## 10. Pi health

`vcgencmd measure_temp`, `vcgencmd get_throttled` (`0x0` is perfect), `vcgencmd pmic_read_adc
EXT5V_V` (input, want ≈ 5.1 V). Throttle bits, right now: `0x1` under-voltage, `0x2` ARM frequency
capped, `0x4` throttled, `0x8` soft temperature limit; `0x10000`–`0x80000` = same, since boot.
`0x50000` → PSU or cable; `0x80008` → heat, check the cooler. DokWalt reads it every 60 s and shows
a non-zero value in `doctor` and `top`. The throttling alert is always on (fires while the Pi is
throttled or under-voltage *now*); the temperature alert defaults to 80 °C:
`dokwalt alerts:set temperature=75`.

## 11. Backups and restore

Back up `/var/lib/dokwalt/dokwalt.db` (apps, releases, domains, encrypted config), `secret.key`
(without it config cannot be decrypted), the Docker volume `dokwalt-caddy-data` (certificates and
ACME account: no re-issuance, no rate limits after a restore) and the `dw-*` volumes (databases,
uploads). Config values exist nowhere else on disk; `caddy/config/caddy.json` is regenerated.
```bash
sudo apt install -y sqlite3; B=/srv/backup/$(date +%F); sudo mkdir -p $B
sudo sqlite3 /var/lib/dokwalt/dokwalt.db ".backup $B/dokwalt.db"
sudo cp /var/lib/dokwalt/secret.key $B/
for v in dokwalt-caddy-data $(docker volume ls -q --filter name=dw-); do
  docker run --rm -v $v:/v:ro -v $B:/b alpine tar czf /b/$v.tgz -C /v .
done
```
A tar of a live database may be inconsistent: also `pg_dump` the URL printed by `dokwalt db:connect
-a blog --service db --tunnel-only`. Copy backups off the Pi; treat `secret.key` like a password.

**Restore onto a fresh Pi:** steps 1–9 (`ssh-keygen -R pi.home` on the Mac), backup in `$B`, then:
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
images are pulled again). Without the state DB: `dokwalt apps:export blog.json` (old server),
`apps:import blog.json` (new one), restore volumes, `deploy`; release history is lost.

## 12. Final checklist

1. `dokwalt doctor` is all ✓ and `dokwalt alerts:test` reaches Discord/Slack.
2. The site opens on your phone **on mobile data** (home Wi-Fi can be fooled by hairpin NAT).
3. **Hard-reboot test**, from the Mac: `ssh -t pi.home sudo reboot`, then time
   `until curl -sfo /dev/null https://blog.example.com; do sleep 2; done`. Must be < 2 min.
4. **Power-cut test:** pull the plug while a page loads, wait 10 s, plug back in, same loop, then
   `dokwalt doctor`, `dokwalt releases` (a deploy cut mid-way shows `failed`) and on the Pi
   `sudo sqlite3 /var/lib/dokwalt/dokwalt.db 'PRAGMA integrity_check'` (must print `ok`).
5. Optional watchdog test (kernel crash; the Pi must come back): `echo c | sudo tee /proc/sysrq-trigger`.

See also: `dokwalt docs reboot-recovery`, `dokwalt docs security`, `dokwalt docs vps`,
`dokwalt docs troubleshooting`.
