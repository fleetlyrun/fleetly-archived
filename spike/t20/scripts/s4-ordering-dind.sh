#!/bin/sh
# spike/t20/scripts/s4-ordering-dind.sh — Spike ④ 宿主编排（单 dind + fleetlyd 全链）。
#
# 形态：宿主交叉编译 fleetlyd+fleetly（linux/amd64）→ 单特权 dind → 内部
# swarm init → fleetlyd 起服 → curl helper 注册 founder + 铸机具令牌 →
# dind 内 fleetly 全链部署三服务 fixture + 观测（见 in-s4-ordering.sh）。
# 用法（Git Bash，仓库根目录）：sh spike/t20/scripts/s4-ordering-dind.sh
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

SELF=$(printf '%s' "$0" | tr '\\' '/')
ROOT=$(CDPATH= cd -- "$(dirname -- "$SELF")/../../.." && pwd)
. "$ROOT/spike/t20/scripts/lib.sh"
cd "$ROOT" || t20_die 'cd repo root'

# 独立网段/网桥（避免与其它 spike 打架）。
T20_BR_NET=t20-s4-br
T20_BR_SUBNET=10.242.0.0/24
export T20_BR_NET T20_BR_SUBNET

t20_need_docker
t20_init_run_dir s4
DIND=t20-s4
DIND_IP=10.242.0.10
CURLER=t20-s4-curl
CURL_IMAGE='curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69'
ALPINE='alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc'
VERSION=v0.3.0-t20s4
INNER="$ROOT/spike/t20/scripts/in-s4-ordering.sh"
SUMMARY="$T20_RUN_DIR/s4-summary.txt"

say() { printf '%s\n' "$*" | tee -a "$SUMMARY"; }
m() { docker exec "$DIND" "$@"; }

cleanup() {
    docker rm -f "$CURLER" >/dev/null 2>&1 || true
    t20_teardown
}
trap cleanup EXIT INT TERM

TMP=$(mktemp -d) || t20_die 'mktemp'
case "$TMP" in
/*)
    if command -v cygpath >/dev/null 2>&1; then
        TMP=$(cygpath -m "$TMP") || t20_die 'cygpath -m'
    fi
    ;;
esac

say "=== build fleetlyd+fleetly ($VERSION) ==="
(
    cd "$ROOT" &&
        GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
            go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
            -o "$TMP/fleetlyd" ./cmd/fleetlyd &&
        GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
            go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
            -o "$TMP/fleetly" ./cmd/fleetly
) >>"$SUMMARY" 2>&1 || t20_die 'go build failed'
say 'binaries built'

t20_bridge_up
t20_dind_up "$DIND" "$DIND_IP" s4

say '=== stage binaries + fixtures + config ==='
m mkdir -p /opt/fleetly/bin /opt/fleetly/etc /var/lib/fleetly || t20_die 'mkdir stage'
t20_stage "$DIND" "$TMP/fleetlyd" /opt/fleetly/bin/fleetlyd
t20_stage "$DIND" "$TMP/fleetly" /opt/fleetly/bin/fleetly
t20_stage "$DIND" "$INNER" /opt/in-s4-ordering.sh
t20_stage "$DIND" "$ROOT/spike/t20/fixtures/ord1-chain.yaml" /opt/fleetly/ord1-chain.yaml
t20_stage "$DIND" "$ROOT/spike/t20/fixtures/ord2-broken.yaml" /opt/fleetly/ord2-broken.yaml
t20_stage "$DIND" "$ROOT/spike/t20/fixtures/ord3-depends-on.yaml" /opt/fleetly/ord3-depends-on.yaml
m chmod +x /opt/fleetly/bin/fleetlyd /opt/fleetly/bin/fleetly || t20_die 'chmod'

cat >"$T20_RUN_DIR/config.yaml" <<'EOF'
addr: "0.0.0.0:8420"
grpc:
  addr: "127.0.0.1:8421"
state:
  db_path: "/var/lib/fleetly/fleetly.db"
secrets:
  key_path: "/var/lib/fleetly/fleetly.key"
build:
  cache_dir: "/var/lib/fleetly/build-cache"
  artifacts_dir: "/var/lib/fleetly/build-artifacts"
logs:
  dir: "/var/lib/fleetly/fleetly-logs"
ingress:
  token_file: "/var/lib/fleetly/fleetly-ingress.token"
  cert_dir: "/var/lib/fleetly/fleetly-certs"
  acme:
    enabled: false
engine:
  deploy_timeout_seconds: 60
  observe_seconds: 5
  replicas_below_seconds: 5
  poll_seconds: 1
  drift_interval_seconds: 3600
git:
  enabled: false
logging:
  level: info
EOF
t20_stage "$DIND" "$T20_RUN_DIR/config.yaml" /opt/fleetly/etc/config.yaml

say '=== pre-pull fixture image + swarm init + fleetlyd boot ==='
m docker pull -q "$ALPINE" >>"$SUMMARY" 2>&1 || t20_die 'pre-pull alpine'
m docker swarm init --advertise-addr eth0 >>"$SUMMARY" 2>&1 || t20_die 'swarm init'
m sh -c 'cd /var/lib/fleetly && nohup /opt/fleetly/bin/fleetlyd -c /opt/fleetly/etc/config.yaml > /tmp/fleetlyd.log 2>&1 & echo $! > /var/run/fleetlyd.pid'
i=0
while :; do
    if m sh -c 'wget -q -T 3 -O /dev/null http://127.0.0.1:8420/healthz/liveness'; then break; fi
    i=$((i + 2))
    [ "$i" -ge 90 ] && t20_die 'fleetlyd not live within 90s'
    sleep 2
done
say 'fleetlyd live (liveness 200)'

say '=== register founder + mint machine token (curl helper on bridge) ==='
docker run -d --name "$CURLER" --network "$T20_BR_NET" "$CURL_IMAGE" sleep 100000 >/dev/null ||
    t20_die "run $CURLER"
docker exec "$CURLER" curl -s -o /dev/null "http://$DIND_IP:8420/healthz/liveness" ||
    t20_die 'curl helper cannot reach fleetlyd REST face'
docker exec "$CURLER" curl -s -c /tmp/jar -X POST "http://$DIND_IP:8420/v1/auth/register" \
    -H 'Content-Type: application/json' \
    -d '{"email":"founder@t20.test","password":"t20-founder-pass","display_name":"T20 Founder"}' \
    >"$T20_RUN_DIR/s4-register.json" 2>&1 || t20_die 'founder register'
TOKEN=$(docker exec "$CURLER" curl -s -b /tmp/jar -X POST "http://$DIND_IP:8420/v1/tokens" \
    -H 'Content-Type: application/json' \
    -d '{"machine":true,"note":"t20 s4 machine token","scopes":["admin"]}' |
    grep -oE '"token": ?"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "$TOKEN" ] || t20_die 'machine token mint failed'
say 'founder registered; machine token minted'

say '=== run inner ordering observation ==='
docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_PROJECT=founder/default \
    -e FLEETLY_TOKEN="$TOKEN" "$DIND" sh /opt/in-s4-ordering.sh \
    >"$T20_RUN_DIR/s4-inner.log" 2>&1
INNER_RC=$?
say "inner rc=$INNER_RC (log: $T20_RUN_DIR/s4-inner.log)"

say '=== collect artifacts ==='
for f in s4-report.txt s4-events-ord1.log s4-events-ord2.log s4-ord1-snapshots.txt \
    s4-ord2-snapshots.txt s4-deploy-ord1.log s4-deploy-ord2.log \
    s4-ord1-deployments.json s4-ord2-deployments.json s4-validate.txt s4-event-watch.txt; do
    docker exec "$DIND" cat "/tmp/$f" >"$T20_RUN_DIR/$f" 2>&1 || true
done
docker exec "$DIND" cat /tmp/fleetlyd.log >"$T20_RUN_DIR/s4-fleetlyd.log" 2>&1 || true
say "artifacts collected: $(ls "$T20_RUN_DIR" | wc -l | tr -d ' ') files"
say '=== S4 DONE ==='
