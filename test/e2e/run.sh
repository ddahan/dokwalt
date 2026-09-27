#!/usr/bin/env bash
# End-to-end test: installs DokWalt on a throwaway Linux server (a privileged
# Debian container running systemd + Docker + sshd) and exercises the real
# workflows: deploy, zero downtime under load, config, rollback, pipeline,
# failed deploy, db tunnel, reboot and power-cut recovery.
#
#   make e2e            # build + run
#   KEEP=1 make e2e     # keep the server container afterwards
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
E2E=$ROOT/test/e2e
WORK=$E2E/work
D=$ROOT/bin/dokwalt
SERVER=dw-e2e
ARCH=$(uname -m); [[ $ARCH == arm64 || $ARCH == aarch64 ]] && ARCH=arm64 || ARCH=amd64

pass() { printf '\033[32m✓ %s\033[0m\n' "$*"; }
fail() { printf '\033[31m✗ %s\033[0m\n' "$*"; exit 1; }
step() { printf '\n\033[35m◆ %s\033[0m\n' "$*"; }
get()  { curl -sk --max-time 3 --resolve "$1:18443:127.0.0.1" "https://$1:18443${2:-/}"; }

cleanup() {
  [[ -n "${KEEP:-}" ]] && { echo "Server kept: docker exec -it $SERVER bash"; return; }
  docker rm -f $SERVER >/dev/null 2>&1 || true
  docker volume rm $SERVER-docker $SERVER-containerd >/dev/null 2>&1 || true
  # Images built on this machine by the test deploys.
  docker images --format '{{.Repository}}:{{.Tag}}' | grep '^dokwalt/shop-' | xargs docker rmi >/dev/null 2>&1 || true
}
trap cleanup EXIT

step "Preparing a throwaway server"
mkdir -p "$WORK"
[[ -f $E2E/id_test ]] || ssh-keygen -q -t ed25519 -N "" -f "$E2E/id_test" -C dokwalt-e2e
docker build -q -f "$E2E/Dockerfile.server" -t dokwalt-e2e-server "$E2E" >/dev/null
cleanup
docker run -d --name $SERVER --hostname $SERVER --privileged \
  -v $SERVER-docker:/var/lib/docker -v $SERVER-containerd:/var/lib/containerd \
  -p 12222:22 -p 18080:80 -p 18443:443 dokwalt-e2e-server >/dev/null
for _ in $(seq 1 30); do docker exec $SERVER systemctl is-active -q ssh docker 2>/dev/null && break; sleep 1; done
cat > "$WORK/ssh_config" <<EOF
Host $SERVER
  HostName 127.0.0.1
  Port 12222
  User deploy
  IdentityFile $E2E/id_test
  IdentitiesOnly yes
  UserKnownHostsFile $WORK/known_hosts
  StrictHostKeyChecking accept-new
EOF
rm -f "$WORK/known_hosts" "$WORK/config.json"
export DOKWALT_SSH_CONFIG=$WORK/ssh_config DOKWALT_CONFIG=$WORK/config.json
pass "server container up"

step "server init"
$D server init $SERVER --name e2e --email e2e@example.com --binary "$ROOT/bin/dokwalt_linux_$ARCH" >/dev/null
for _ in $(seq 1 90); do docker exec $SERVER docker inspect -f '{{.State.Running}}' dokwalt-caddy 2>/dev/null | grep -q true && break; sleep 1; done
pass "installed; proxy running"

cd "$E2E/app"
step "First deploy"
$D apps:create shop >/dev/null
$D config:set GREETING=hello >/dev/null
APP_VERSION=v1 $D deploy -m "v1" >/dev/null
$D domains:add shop.localhost --service web >/dev/null
sleep 2
get shop.localhost | grep -q "v1 .*greeting=hello | cache=ok" || fail "v1 not served: $(get shop.localhost)"
pass "v1 served over HTTPS with config and stateful cache"

step "Zero-downtime deploy under load"
( end=$((SECONDS+45)); ok=0; bad=0
  while [ $SECONDS -lt $end ]; do
    if get shop.localhost | grep -q greeting; then ok=$((ok+1)); else bad=$((bad+1)); fi
  done; echo "$ok $bad" > "$WORK/load" ) &
sleep 2
APP_VERSION=v2 $D deploy -m "v2" >/dev/null
wait
read -r ok bad < "$WORK/load"
[[ $bad == 0 ]] || fail "$bad failed requests out of $((ok+bad)) during deploy"
get shop.localhost | grep -q "^v2 " || fail "v2 not live"
pass "v2 live; $ok requests, 0 failures"

step "Unchanged redeploy skips upload"
out=$(APP_VERSION=v2 $D deploy 2>&1)
grep -q "Images unchanged" <<<"$out" || fail "images re-uploaded"
pass "nothing uploaded"

step "Config change is a zero-downtime release; database untouched"
before=$(docker exec $SERVER docker inspect -f '{{.State.StartedAt}}' dw-shop-production-data-cache-1)
$D config:set GREETING=bonjour >/dev/null
get shop.localhost | grep -q "greeting=bonjour" || fail "config not applied"
after=$(docker exec $SERVER docker inspect -f '{{.State.StartedAt}}' dw-shop-production-data-cache-1)
[[ $before == "$after" ]] || fail "stateful service was restarted by a config change"
pass "config applied, cache kept running"

step "Rollback"
$D rollback v1 >/dev/null
get shop.localhost | grep -q "^v1 .*greeting=bonjour" || fail "rollback failed: $(get shop.localhost)"
pass "back on v1 images with current config"

step "Failed deploy keeps the current version"
rm -rf "$WORK/broken" && cp -r "$E2E/app" "$WORK/broken"
sed -i.bak 's/^print("listening on 8000", flush=True)/raise SystemExit("boom")/' "$WORK/broken/app.py"
if (cd "$WORK/broken" && $D deploy -a shop -m broken > "$WORK/broken.log" 2>&1); then fail "broken deploy succeeded"; fi
grep -q boom "$WORK/broken.log" || fail "crash logs not shown"
get shop.localhost | grep -q "^v1 " || fail "site down after failed deploy"
pass "deploy failed with logs; v1 still serving"

step "Pipeline & promote"
$D pipeline:enable >/dev/null
$D config:set -s staging GREETING=staging >/dev/null
$D domains:add staging.shop.localhost -s staging --service web >/dev/null
APP_VERSION=v3 $D deploy -s staging -m v3 >/dev/null
get staging.shop.localhost | grep -q "^v3 .*greeting=staging" || fail "staging not serving v3"
$D promote >/dev/null
get shop.localhost | grep -q "^v3 .*greeting=bonjour" || fail "promote failed: $(get shop.localhost)"
pass "staging v3 promoted with production config"

step "DB tunnel"
$D db:connect --tunnel-only --port 16399 >/dev/null 2>&1 &
TPID=$!
sleep 3
printf 'PING\r\nQUIT\r\n' | nc -w 3 127.0.0.1 16399 | grep -q PONG || fail "redis tunnel"
kill $TPID 2>/dev/null || true
pass "redis answered PONG through SSH"

step "Reboot"
docker restart $SERVER >/dev/null
for i in $(seq 1 120); do get shop.localhost | grep -q greeting && break; sleep 1; done
get shop.localhost | grep -q "^v3 " || fail "site not back after reboot"
pass "site back after reboot in ~${i}s, no manual action"

step "Power cut"
docker kill $SERVER >/dev/null && docker start $SERVER >/dev/null
for i in $(seq 1 120); do get shop.localhost | grep -q greeting && break; sleep 1; done
get shop.localhost | grep -q "^v3 " || fail "site not back after power cut"
pass "site back after power cut in ~${i}s"

step "Doctor & footprint"
$D doctor >/dev/null || true
rss=$(docker exec $SERVER sh -c 'grep VmRSS /proc/$(pidof dokwalt)/status' | awk '{print $2}')
echo "daemon RSS: $((rss/1024)) MiB"
[[ $rss -lt 40960 ]] || fail "daemon uses more than 40 MiB"
pass "all end-to-end checks passed"
