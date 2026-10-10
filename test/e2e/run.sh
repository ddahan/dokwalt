#!/usr/bin/env bash
# End-to-end test: installs DokWalt on a throwaway Linux server (a privileged
# Debian container running systemd + Docker + sshd) and exercises the real
# workflows: deploy, zero downtime under load, config, rollback, pipeline,
# failed deploy, db tunnel, shared Postgres, off-site backups (S3), reboot
# and power-cut recovery.
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

step "Shared Postgres (x-dokwalt.uses) and db:backup"
srv() { docker exec $SERVER "$@"; }
webof() { srv docker ps -q --filter "label=dokwalt.app=$1" --filter label=dokwalt.service=web --filter label=dokwalt.role=app | head -1; }
mkdir -p "$WORK/pg" "$WORK/blog"
cat > "$WORK/pg/compose.yaml" <<'EOF'
services:
  db:
    image: postgres:18-alpine
    environment:
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes: [pgdata:/var/lib/postgresql]
    healthcheck: {test: [CMD-SHELL, pg_isready -U postgres], interval: 2s, retries: 30}
volumes:
  pgdata:
EOF
blog_compose() { # $1: extra top-level YAML
  cat > "$WORK/blog/compose.yaml" <<EOF
$1
services:
  web:
    image: alpine:3.20
    command: [sleep, infinity]
    environment:
      DATABASE_URL: postgres://blog:\${BLOG_DB_PASSWORD}@pg:5432/blog
EOF
}
blog_compose "x-dokwalt: {uses: [pg]}"
(cd "$WORK/blog" && $D apps:create blog >/dev/null && $D config:set BLOG_DB_PASSWORD=blogpw >/dev/null)
out=$(cd "$WORK/blog" && $D deploy 2>&1) && fail "deploy succeeded before the provider exists"
grep -q 'app "pg"' <<<"$out" || fail "unclear error without provider: $out"
(cd "$WORK/pg" && $D apps:create pg >/dev/null && $D config:set POSTGRES_PASSWORD=rootpw >/dev/null && $D deploy >/dev/null)
# SQL piped through `dokwalt exec` (stdin, no TTY), as infra provisioning scripts do.
printf "CREATE ROLE blog LOGIN PASSWORD 'blogpw';\nCREATE DATABASE blog OWNER blog;\n" \
  | (cd "$WORK/pg" && $D exec --service db -- psql -qU postgres -v ON_ERROR_STOP=1) >/dev/null || fail "piped exec (provisioning)"
printf "CREATE TABLE posts (id int); INSERT INTO posts VALUES (1);\n" \
  | (cd "$WORK/pg" && $D exec --service db -- psql -qU blog -d blog -v ON_ERROR_STOP=1) >/dev/null || fail "piped exec (blog table)"
(cd "$WORK/blog" && $D deploy >/dev/null) || fail "consumer deploy failed"
srv docker exec "$(webof blog)" nc -z -w 3 pg 5432 || fail "consumer can't reach the shared postgres"
srv docker exec "$(webof blog)" nc -z -w 3 cache 6379 2>/dev/null && fail "consumer reaches another app's service"
srv docker exec "$(webof shop)" python -c "import socket; socket.create_connection(('pg', 5432), 3)" 2>/dev/null \
  && fail "non-consumer reaches the shared postgres"
pass "blog reaches pg:5432; other apps stay isolated"

(cd "$WORK/blog" && $D db:backup -o "$WORK/blog.dump" >/dev/null) || fail "db:backup failed"
docker run --rm -v "$WORK/blog.dump:/d.dump:ro" postgres:18-alpine pg_restore -l /d.dump | grep -q "TABLE DATA public posts" \
  || fail "backup doesn't contain the posts table"
pass "db:backup downloaded the blog database with the app's own role"

sed -i.bak 's/interval: 2s/interval: 3s/' "$WORK/pg/compose.yaml" # recreates the provider container
(cd "$WORK/pg" && $D deploy >/dev/null)
srv docker exec "$(webof blog)" nc -z -w 3 pg 5432 || fail "link lost after the provider was recreated"
out=$($D apps:destroy pg --confirm pg 2>&1) && fail "destroyed a provider still in use"
grep -q "used by blog/production" <<<"$out" || fail "unclear destroy refusal: $out"
pass "link survives a provider redeploy; in-use provider can't be destroyed"

step "Off-site backups (S3: SeaweedFS on the server stands in for R2)"
echo '{"identities":[{"name":"dokwalt","credentials":[{"accessKey":"dokwalt","secretKey":"s3-secret"}],"actions":["Admin","Read","Write","List"]}]}' \
  | docker exec -i $SERVER sh -c 'cat > /root/s3.json'
srv docker run -d --name s3 -p 127.0.0.1:8333:8333 -v /root/s3.json:/s3.json:ro \
  chrislusf/seaweedfs server -dir=/data -s3 -s3.config=/s3.json >/dev/null
for _ in $(seq 1 60); do echo "s3.bucket.create -name backups" | docker exec -i $SERVER docker exec -i s3 weed shell 2>&1 | grep -qi error || break; sleep 1; done
setup() { echo "$1" | $D backup:setup --endpoint http://127.0.0.1:8333 --region us-east-1 --bucket backups --access-key dokwalt \
  --secret-key-stdin --keep-daily 1 --keep-weekly 1 2>&1; }
# A wrong secret key is refused (this also waits for the S3 gateway to start).
for _ in $(seq 1 60); do out=$(setup wrong) && fail "setup accepted a wrong secret key"; grep -q SignatureDoesNotMatch <<<"$out" && break; sleep 1; done
grep -q "can't write to bucket backups: .*SignatureDoesNotMatch" <<<"$out" || fail "unclear setup error: $out"
out=$(setup s3-secret) || fail "backup:setup failed: $out"
# An old complete backup and an old interrupted one: retention removes both.
old=/buckets/backups/dokwalt/$SERVER
srv docker exec s3 mkdir -p /tmp/o
echo '{"id":"20200101T030000Z"}' | docker exec -i $SERVER docker exec -i s3 tee /tmp/o/manifest.json >/dev/null
echo partial | docker exec -i $SERVER docker exec -i s3 tee /tmp/o/blog.dump >/dev/null
srv docker exec s3 weed filer.copy /tmp/o/manifest.json http://localhost:8888$old/20200101T030000Z/ >/dev/null
srv docker exec s3 weed filer.copy /tmp/o/blog.dump http://localhost:8888$old/20200102T030000Z/pg-production-db/ >/dev/null
$D backup:now >/dev/null || fail "backup:now failed"
files=$($D backups latest --json)
for f in dokwalt.db pg-production-db/globals.sql pg-production-db/blog.dump pg-production-db/postgres.dump; do
  grep -q "\"path\": \"$f\"" <<<"$files" || fail "backup is missing $f: $files"
done
# Empty folders may linger in the filer: check the files are gone.
left=$(printf 'fs.ls %s\n' $old/20200101T030000Z $old/20200102T030000Z/pg-production-db | docker exec -i $SERVER docker exec -i s3 weed shell 2>&1)
grep -qE "manifest.json|blog.dump" <<<"$left" && fail "old backups not pruned: $left"
$D server info | grep -q "Backups.*ok" || fail "server info doesn't show the last backup"
pass "nightly backup set: dokwalt.db, roles, every database; old backups pruned"

rm -rf "$WORK/dl"
$D backup:download latest pg-production-db/blog.dump -o "$WORK/dl" >/dev/null || fail "backup:download failed"
docker run --rm -v "$WORK/dl/blog.dump:/d.dump:ro" postgres:18-alpine pg_restore -l /d.dump | grep -q "TABLE DATA public posts" \
  || fail "downloaded backup doesn't contain the posts table"
psql_pg() { srv docker exec -i dw-pg-production-data-db-1 psql -XAtqU postgres -d blog -c "$1"; }
psql_pg "DROP TABLE posts" >/dev/null
$D backup:restore latest --database blog --confirm blog >/dev/null || fail "backup:restore failed"
[[ $(psql_pg "SELECT count(*) FROM posts") == 1 ]] || fail "posts not restored"
[[ $(psql_pg "SELECT tableowner FROM pg_tables WHERE tablename = 'posts'") == blog ]] || fail "restored table lost its owner"
$D backup:restore latest pg-production-db/globals.sql --confirm roles >/dev/null || fail "roles restore failed"
pass "download checks out; a dropped table comes back with its owner"

step "Reboot"
docker restart $SERVER >/dev/null
for i in $(seq 1 120); do get shop.localhost | grep -q greeting && break; sleep 1; done
get shop.localhost | grep -q "^v3 " || fail "site not back after reboot"
for _ in $(seq 1 60); do srv docker exec "$(webof blog)" nc -z -w 2 pg 5432 2>/dev/null && break; sleep 1; done
srv docker exec "$(webof blog)" nc -z -w 3 pg 5432 || fail "shared postgres unreachable after reboot"
pass "site back after reboot in ~${i}s, no manual action; shared postgres linked"

step "Dropping x-dokwalt.uses unlinks the provider"
blog_compose ""
(cd "$WORK/blog" && $D deploy >/dev/null)
srv docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' dw-pg-production-data-db-1 | grep -q dw-blog-production \
  && fail "provider still on the consumer's network"
pass "pg left blog's network"

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
