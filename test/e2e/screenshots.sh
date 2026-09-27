#!/usr/bin/env bash
# Regenerates the README screenshots from a real run: a throwaway server
# (same image as the e2e suite), two apps, live traffic, then captures the
# dashboard and a deploy as ANSI and renders them with charmbracelet/freeze.
#
#   make build && ./test/e2e/screenshots.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
E2E=$ROOT/test/e2e
WORK=$E2E/work/shots
OUT=$ROOT/docs/assets
D=$ROOT/bin/dokwalt
SERVER=dw-shots
ARCH=$(uname -m); [[ $ARCH == arm64 || $ARCH == aarch64 ]] && ARCH=arm64 || ARCH=amd64
command -v freeze >/dev/null || { echo "install freeze: brew install charmbracelet/tap/freeze"; exit 1; }

cleanup() {
  if [[ -n "${LOAD:-}" ]]; then kill "$LOAD" 2>/dev/null || true; fi
  docker rm -f $SERVER >/dev/null 2>&1 || true
  docker volume rm $SERVER-docker $SERVER-containerd >/dev/null 2>&1 || true
  docker images --format '{{.Repository}}:{{.Tag}}' | grep -E '^dokwalt/(shop|blog)-' | xargs docker rmi >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup
mkdir -p "$WORK" "$OUT"
[[ -f $E2E/id_test ]] || ssh-keygen -q -t ed25519 -N "" -f "$E2E/id_test" -C dokwalt-e2e
docker build -q -f "$E2E/Dockerfile.server" -t dokwalt-e2e-server "$E2E" >/dev/null
docker run -d --name $SERVER --hostname prod-1 --privileged \
  -v $SERVER-docker:/var/lib/docker -v $SERVER-containerd:/var/lib/containerd \
  -p 14222:22 -p 14443:443 dokwalt-e2e-server >/dev/null
for _ in $(seq 1 30); do docker exec $SERVER systemctl is-active -q ssh docker 2>/dev/null && break; sleep 1; done
cat > "$WORK/ssh_config" <<EOF
Host prod-1
  HostName 127.0.0.1
  Port 14222
  User deploy
  IdentityFile $E2E/id_test
  IdentitiesOnly yes
  UserKnownHostsFile $WORK/known_hosts
  StrictHostKeyChecking accept-new
EOF
rm -f "$WORK/known_hosts" "$WORK/config.json"
export DOKWALT_SSH_CONFIG=$WORK/ssh_config DOKWALT_CONFIG=$WORK/config.json

echo "→ installing"
$D server init prod-1 --name prod --email you@example.com --binary "$ROOT/bin/dokwalt_linux_$ARCH" >/dev/null
for app in shop blog; do
  rm -rf "$WORK/$app" && cp -r "$E2E/app" "$WORK/$app"
  # A DokWalt-native compose file declares its port with expose, not ports.
  sed -i.bak 's/ports: \["8000:8000"\]/expose: ["8000"]/' "$WORK/$app/compose.yaml"
done

echo "→ deploying demo apps"
(cd "$WORK/blog" && $D apps:create blog >/dev/null && $D config:set GREETING=hello >/dev/null \
  && for v in v1 v2 v3; do APP_VERSION=$v $D deploy -m "Post $v" >/dev/null; done \
  && $D domains:add blog.example.test --service web >/dev/null \
  && $D domains:redirect www.blog.example.test blog.example.test >/dev/null)
(cd "$WORK/shop" && $D apps:create shop >/dev/null && $D config:set GREETING=bonjour >/dev/null \
  && APP_VERSION=v1 $D deploy -m "Launch the shop" >/dev/null \
  && APP_VERSION=v2 $D deploy -m "New pricing page" >/dev/null \
  && $D domains:add shop.example.test --service web >/dev/null \
  && $D pipeline:enable >/dev/null && $D config:set -s staging GREETING=staging >/dev/null \
  && $D domains:add staging.shop.example.test -s staging --service web >/dev/null \
  && APP_VERSION=v3 $D deploy -s staging -m "Checkout redesign" >/dev/null)

echo "→ generating traffic and metrics history (~3 minutes)"
( while true; do
    for h in shop.example.test shop.example.test shop.example.test blog.example.test staging.shop.example.test; do
      curl -sk -o /dev/null --max-time 2 --resolve "$h:14443:127.0.0.1" "https://$h:14443/" || true
    done
    sleep 0.$((RANDOM % 4))
  done ) &
LOAD=$!
sleep 170

echo "→ capturing the dashboard"
DOKWALT_SNAPSHOT=112x22 $D -a shop > "$WORK/dashboard.ansi"

echo "→ capturing a deploy"
cd "$WORK/shop"
APP_VERSION=v4 python3 - "$D" > "$WORK/deploy.ansi" <<'PY'
import os, pty, re, sys
env = dict(os.environ, TERM="xterm-256color", COLORTERM="truecolor", COLUMNS="100")
pid, fd = pty.fork()
if pid == 0:
    os.execve(sys.argv[1], ["dokwalt", "deploy", "-m", "Faster checkout"], env)
out = []
while True:
    try:
        data = os.read(fd, 65536)
    except OSError:
        break
    if not data:
        break
    out.append(data)
os.waitpid(pid, 0)
raw = b"".join(out).decode("utf-8", "replace")
raw = re.sub(r"\x1b\][^\x07\x1b]*(\x07|\x1b\\)", "", raw)   # OSC queries
raw = re.sub(r"\x1b\[\d*n|\x1b\[\?25[lh]", "", raw)          # status queries, cursor
# Replay "cursor up N + clear" to get the final screen, keeping colors.
lines, row = [""], 0
for tok in re.findall(r"\x1b\[\d+F\x1b\[J|\r\n|\n|\r|\x1b\[[0-9;]*m|[^\x1b\r\n]+|\x1b", raw):
    if tok.endswith("J") and tok.startswith("\x1b["):
        n = int(re.match(r"\x1b\[(\d+)F", tok).group(1))
        row = max(0, row - n); del lines[row:]; lines.append("")
    elif tok in ("\n", "\r\n"):
        row += 1
        if row >= len(lines): lines.append("")
    elif tok == "\r":
        lines[row] = ""
    else:
        lines[row] += tok
print("$ dokwalt deploy -m \"Faster checkout\"")
print("\n".join(l for l in lines if re.sub(r"\x1b\[[0-9;]*m", "", l).strip()))
PY

echo "→ rendering images"
style=(--window --padding 24,28 --margin 40 --border.radius 12 --border.width 1 --border.color "#2a2a3a"
       --shadow.blur 24 --shadow.y 12 --background "#0b0b12" --font.size 14 --line-height 1.25)
freeze "$WORK/dashboard.ansi" -l ansi "${style[@]}" -o "$OUT/dashboard.png"
freeze "$WORK/deploy.ansi" -l ansi "${style[@]}" -o "$OUT/deploy.png"
# freeze renders at 4x; 2x is plenty for README images.
for f in dashboard deploy; do
  w=$(sips -g pixelWidth "$OUT/$f.png" | awk '/pixelWidth/{print $2}')
  sips --resampleWidth $((w / 2)) "$OUT/$f.png" >/dev/null
done
echo "✓ $OUT/dashboard.png, $OUT/deploy.png"
