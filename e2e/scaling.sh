#!/bin/sh
# e2e/scaling.sh — E6 W5-S4 自动扩缩端到端（单节点 dind 形态；W5-S1
# D-V3W5-2 的真机闭环，metrics.sh 同骨架——编排自足、镜像钉 digest）：
#
#   交叉编译 linux/amd64 fleetlyd+fleetly（或复用 SCL_BIN_DIR）→ 宿主 bridge
#   上起一个特权 dind（私网 10.220.0.0/24，镜像钉 digest）→ swarm init +
#   fleetlyd 起服 → 全部断言经 dind 内的 docker/fleetly CLI/REST 驱动：
#
#   前置链：metrics mode on 三件收敛（SCL-A*）→ 部署 CPU 打压应用
#   （alpine sha1sum 循环 ×2 副本 + cpus 限额，SCL-B*）与带卷应用
#   （stateful 缩容护栏的载体）→ 双服务落策略（target 低阈值，SCL-C*）。
#
#   **指标源（真机形态注记，2026-09-25 实证）**：cAdvisor v0.55.1 在 dockerd
#   托管 containerd 形态下（dind 与系统 containerd 宿主同病，storage driver
#   无关——overlay2 dind 实证同缺席）只暴露 bundle-path 容器标签，swarm
#   归属标签缺席（e2e/metrics.sh A13 / observability §6 挂账的同源上游限
#   制）。autoscaler 的 per-service 采样查询因此结构性拿不到真实序列——
#   本套件经 **VM 原生 import API**（POST /api/v1/import/prometheus，
#   127.0.0.1:8428——host 网络任务与 dind 同 netns）注入带 swarm 归属标签
#   的合成计数序列驱动评估链。评估链本身（策略门槛→VM 采样→HPA 反解→
#   平台副本写通道→覆盖行→事件/审计/漂移钉定）全部真实；合成的只有指标
#   数据（fixture 注记；staging 真机同形态——.w3out/staging-v03/verify-w5.sh）。
#
#   SCL-A1-A3 metrics opt-in：mode set on → 三件 Running → nodes_reporting
#      1/1（抓取面收敛——瞬时查询可见注入序列的前提）。
#   SCL-B1-B2 打压应用（burn，×2 副本）与带卷应用（vault，×1 副本——受控
#      子集拒绝「命名卷 + replicas>1」的组合，stateful 扩缩语义由 autoscaler
#      的运行期覆盖层承担）部署 succeeded。
#   SCL-C1-C3 策略落库与读面回放：burn min1/max4/cpu20/cooldown180、vault
#      min1/max2/cpu20/cooldown180（target 低阈值——注入水位一拍即过扩带）。
#   SCL-D1 诚实无数据：注入前 scaling.no_data 一次性披露（真实打压负载
#      在跑而归属标签缺席 = no_series 判据的真实触发形态）。
#   SCL-E1 扩容：注入 +3.0/s 计数斜率（2×1.5 核打满形态，cpu_pct=100%>
#      22% 扩带）→ 副本 2→4（ceil(2×100/20)=10 夹逼 max4）——收敛循环 30s
#      频控 + rate 窗，重试窗 420s。
#   SCL-E2 scaling.adjusted 事件载荷（dimension=cpu / replicas_before 2 /
#      after 4 / service 归属）。
#   SCL-E3 策略投影稳定：调整后 scaling show 的 min/max/target/cooldown
#      不变（autoscaler 只写副本覆盖层，不碰策略行）。
#   SCL-F1 冷却窗护栏：斜率降到 +0.5/s（cpu_pct≈8.3%<14% 缩带）后冷却窗
#      （180s）内静态等 70s（≥2 个评估拍）副本保持 4。
#   SCL-F2 冷却过期缩容：+0.5/s 下反解 ceil(4×8.3/20)=2（HPA 比例缩容，
#      不触 min 夹逼）→ 副本 4→2（cur=2 时 pct=16.7% 回到死区——稳定点）。
#   SCL-F3 scaling.adjusted 下行事件载荷（replicas_after=2）。
#   SCL-G0 stateful 只扩不缩·扩向：vault 高水位注入 → 1→2（护栏允许扩）。
#   SCL-G1 stateful 只扩不缩·缩向：低水位注入（缩带判据恒真）冷却过期后
#      副本恒 2——护栏拦住 2→1 的缩容提案（判据真、动作拒）。
#   SCL-G2 vault 无任何下行 scaling.adjusted（护栏的负向面）。
#   SCL-H1 策略移除：apps scaling rm → show 缺席（非零退出）。
#   SCL-I1 metrics off 休眠披露：metrics mode set unset → scaling.dormant
#      一次性事件（vault 的策略行仍在场）。
#
# 断言风格与 e2e/metrics.sh 一致（SCL-x: PASS/FAIL 行 + NL_FAIL 计数 +
# finish）。
# usage: e2e/scaling.sh
# env:
#   SCL_DIND_IMAGE   dind 镜像（默认 docker:29.8.1-dind，钉 digest 与 CI 一致）
#   SCL_SKIP_BUILD   1 = 跳过交叉编译，改用 SCL_BIN_DIR 下的现成二进制
#   SCL_BIN_DIR      SCL_SKIP_BUILD=1 时的二进制来源（需含 fleetlyd 与 fleetly）
#   SCL_VERSION      注入的版本串（默认 v0.3.0-scaling-e2e）
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

# ── 镜像钉 digest（T0-V2.3 供应链；与 e2e/metrics.sh 同源多锚）。
DIND_IMAGE="${SCL_DIND_IMAGE:-docker:29.8.1-dind@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0}"
CURL_IMAGE='curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69'
VM_IMG='victoriametrics/victoria-metrics:v1.152.0@sha256:86ca5fdb6d87d56ba047b044039019ba2bd9042b36e35f6ea34e437b6c825cef'
NODE_EXPORTER_IMG='prom/node-exporter:v1.12.1@sha256:1b4e4438faca4dd7e001dd445d161a4a2091b0fededa84093b3a8dfeae1f1be0'
CADVISOR_IMG='gcr.io/cadvisor/cadvisor:v0.55.1@sha256:3de2bd5203120b866d74a9b283b2ffb8ec382fbf9dc321814700c6ea6f44ec57'
ALPINE_IMG='alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc'
SCL_SKIP_BUILD="${SCL_SKIP_BUILD:-0}"
SCL_BIN_DIR="${SCL_BIN_DIR:-}"
SCL_VERSION="${SCL_VERSION:-v0.3.0-scaling-e2e}"

BR_NET=fleetly-scl-br
BR_SUBNET=10.220.0.0/24
DIND=fleetly-scl-e2e-dind
BURN_APP=stressapp
BURN_SVC=burn
VAULT_APP=vaultapp
VAULT_SVC=data
# 平台服务命名公式（internal/naming.ServiceName）：fleetly-<team>-<prj>-<app>-<svc>。
BURN_SERVICE=fleetly-founder-default-$BURN_APP-$BURN_SVC
VAULT_SERVICE=fleetly-founder-default-$VAULT_APP-$VAULT_SVC
VM_IMPORT_URL='http://127.0.0.1:8428/api/v1/import/prometheus'

NL_FAIL=0
SUITE_DINDS=''
ACTIVE_NET=''

nl() { printf '[scl-e2e %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
pass() { nl "$1: PASS"; }
fail() {
    nl "$1: FAIL ${2:-}"
    # shellcheck disable=SC2034
    NL_FAIL=$((NL_FAIL + 1))
}
assert() { # <name> <0|1> [detail]
    if [ "$2" -eq 0 ]; then pass "$1"; else fail "$1" "${3:-}"; fi
}
fatal() { nl "FATAL $*"; exit 1; }
finish() {
    if [ "$NL_FAIL" -eq 0 ]; then
        nl "SUITE-DONE all-asserts-passed"
        exit 0
    fi
    nl "SUITE-DONE failed-asserts=$NL_FAIL"
    exit 1
}

# poll_until <cap-s> <sh-test...> — 每 2s 重试一条宿主侧测试命令直到 rc=0。
poll_until() {
    cap=$1
    shift
    i=0
    while [ "$i" -lt "$cap" ]; do
        if "$@"; then return 0; fi
        i=$((i + 2))
        sleep 2
    done
    return 1
}

m() { docker exec "$DIND" "$@"; }         # dind 内直跑
msh() { docker exec "$DIND" sh -c "$*"; } # dind 内跑 shell 段
# fcli <args...> — dind 内的 fleetly CLI（gRPC 面 + machine token）。
fcli() {
    docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_PROJECT="$FOUNDER_PROJECT" -e FLEETLY_TOKEN="$SCL_TOKEN" \
        "$DIND" /opt/fleetly/bin/fleetly "$@"
}
# events_grep <pattern> — 文本事件流快照检索（watch 快照即「迄今全部事件」；
# 文本行 = #seq at name subject——事件名层面的断言面）。
events_grep() {
    docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$SCL_TOKEN" \
        "$DIND" sh -c 'timeout 4 /opt/fleetly/bin/fleetly events watch --since-seq 0 > /tmp/scl-events.txt 2>/dev/null; exit 0'
    m grep -q "$1" /tmp/scl-events.txt
}
# events_json_snap — --json 快照落回宿主并反转义（protojson 把 payload 字
# 符串内的引号转义为 \"——sed 反转义后按原始 JSON kv 片段检索）。
events_json_snap() {
    docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$SCL_TOKEN" \
        "$DIND" sh -c 'timeout 4 /opt/fleetly/bin/fleetly events watch --since-seq 0 --json > /tmp/scl-events-raw.txt 2>/dev/null; exit 0'
    m cat /tmp/scl-events-raw.txt >"$TMP/scl-events.json" 2>/dev/null || true
    sed 's/\\"/"/g' "$TMP/scl-events.json" >"$TMP/scl-events-flat.json"
}
# events_json_has <fixed-string> — 反转义快照内的固定串检索。
events_json_has() {
    grep -qF "$1" "$TMP/scl-events-flat.json"
}
# start_import_loop <burn-slope-per-tick> <vault-slope-per-tick> <start-value>
# — 注入循环（dind 内后台进程，10s 一拍；burn/vault 序列各按自身斜率增长）。
# pidfile 管理生命周期（stop 后可换斜率重启——计数起点接续，避免 counter
# reset 被 rate() 判成尖峰）。
stage_import_loop() {
    cat >"$TMP/scl-import.sh" <<EOF
#!/bin/sh
# synthetic counter import loop (fixture — see suite header note)
set -u
burn_slope=\$1
vault_slope=\$2
v=\$3
while :; do
  ts=\$(( \$(date +%s) * 1000 ))
  wget -q -T 5 -O /dev/null --post-data "container_cpu_usage_seconds_total{container_label_com_docker_swarm_service_name=\\"$BURN_SERVICE\\",image=\\"alpine\\"} \$v \$ts" "$VM_IMPORT_URL" 2>/dev/null
  wget -q -T 5 -O /dev/null --post-data "container_cpu_usage_seconds_total{container_label_com_docker_swarm_service_name=\\"$VAULT_SERVICE\\",image=\\"alpine\\"} \$(( \$v / \$(( burn_slope / vault_slope )) )) \$ts" "$VM_IMPORT_URL" 2>/dev/null
  v=\$(( \$v + burn_slope ))
  sleep 10
done
EOF
    stage "$DIND" "$TMP/scl-import.sh" /tmp/scl-import.sh
    m chmod +x /tmp/scl-import.sh || fatal 'chmod import loop'
}
start_import_loop() { # <burn-slope> <vault-slope> <start-value>
    docker exec -d "$DIND" sh -c 'echo $$ > /tmp/scl-import.pid; exec /tmp/scl-import.sh "$1" "$2" "$3"' \
        scl-import "$1" "$2" "$3"
    nl "synthetic series injection running (burn slope $1 per 10s, vault slope $2 per 10s, start value $3)"
}
stop_import_loop() {
    msh 'v=$(cat /tmp/scl-import.pid 2>/dev/null); [ -n "$v" ] && kill "$v" 2>/dev/null; true'
}
burn_replicas() {
    msh "docker service inspect '$BURN_SERVICE' --format '{{.Spec.Mode.Replicated.Replicas}}' 2>/dev/null" | tr -d '[:space:]'
}
vault_replicas() {
    msh "docker service inspect '$VAULT_SERVICE' --format '{{.Spec.Mode.Replicated.Replicas}}' 2>/dev/null" | tr -d '[:space:]'
}

cleanup() {
    for d in $SUITE_DINDS; do
        docker rm -f "$d" >/dev/null 2>&1 || true
    done
    if [ -n "$ACTIVE_NET" ]; then
        docker network rm "$ACTIVE_NET" >/dev/null 2>&1 || true
    fi
    rm -rf "$TMP"
}
on_exit() {
    rc=$?
    if [ "$rc" -ne 0 ] && [ -n "$SUITE_DINDS" ]; then
        nl "suite RED (rc=$rc) — dumping dind log tail"
        docker logs "$DIND" --tail 120 2>&1 | tail -60 || true
    fi
    cleanup
}
trap on_exit EXIT INT TERM

# stage <dind> <local-file> <remote-path> — exec+stdin 直传 + 两侧体积校验 +
# 文本脚本 CR 剥离（busybox ash 不执行 CRLF）。
stage() {
    d=$1
    f=$2
    r=$3
    docker exec -i "$d" sh -c "cat > '$r'" <"$f" || fatal "staging $r"
    hsz=$(wc -c <"$f" | tr -d ' ')
    gsz=$(docker exec "$d" sh -c "wc -c < '$r'" | tr -d ' ')
    [ "$hsz" = "$gsz" ] || fatal "size mismatch for $r: host=$hsz dind=$gsz"
    case "$r" in
    *.sh | *.yaml | *.yml | *Dockerfile)
        docker exec "$d" sed -i 's/\r$//' "$r" || fatal "strip CR from $r"
        ;;
    esac
}

posix_path() { printf '%s' "$1" | tr '\\' '/'; }
SELF=$(posix_path "$0")
ROOT=$(CDPATH= cd -- "$(dirname -- "$SELF")/.." && pwd)
TMP=$(posix_path "$(mktemp -d)")
case "$TMP" in
/*)
    if command -v cygpath >/dev/null 2>&1; then
        TMP=$(cygpath -m "$TMP") || fatal 'cygpath -m on tmpdir'
    fi
    ;;
esac

command -v docker >/dev/null 2>&1 || fatal 'docker not on PATH'
command -v go >/dev/null 2>&1 || SCL_SKIP_BUILD=1

# ─────────────────────────────────────────────────────────────── 构建
if [ "$SCL_SKIP_BUILD" != '1' ]; then
    nl "cross-compiling linux/amd64 fleetlyd+fleetly ($SCL_VERSION)"
    (
        cd "$ROOT" &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$SCL_VERSION" \
                -o "$TMP/fleetlyd" ./cmd/fleetlyd &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$SCL_VERSION" \
                -o "$TMP/fleetly" ./cmd/fleetly
    ) || fatal 'go build failed'
    SCL_BIN_DIR="$TMP"
else
    SCL_BIN_DIR="${SCL_BIN_DIR:?SCL_SKIP_BUILD=1 requires SCL_BIN_DIR}"
    nl "using prebuilt binaries from $SCL_BIN_DIR"
fi
[ -f "$SCL_BIN_DIR/fleetlyd" ] || fatal "fleetlyd missing in $SCL_BIN_DIR"
[ -f "$SCL_BIN_DIR/fleetly" ] || fatal "fleetly missing in $SCL_BIN_DIR"

# ─────────────────────────────────────────────────────── dind 编排准备
leftovers=$(docker ps -aq --filter name=fleetly-scl- 2>/dev/null || true)
if [ -n "$leftovers" ]; then
    nl "WARN removing leftover scl-e2e containers: $leftovers"
    echo "$leftovers" | xargs docker rm -f >/dev/null 2>&1 || true
fi
docker network rm "$BR_NET" >/dev/null 2>&1 || true
docker network create -d bridge --subnet "$BR_SUBNET" "$BR_NET" >/dev/null || fatal "create $BR_NET"
ACTIVE_NET="$BR_NET"

docker rm -f "$DIND" >/dev/null 2>&1 || true
docker run -d --name "$DIND" --privileged --hostname mgr \
    --network "$BR_NET" --ip 10.220.0.10 "$DIND_IMAGE" >/dev/null ||
    fatal "docker run $DIND"
SUITE_DINDS="$DIND"
i=0
while ! docker exec "$DIND" docker info >/dev/null 2>&1; do
    i=$((i + 2))
    if [ "$i" -ge 90 ]; then
        docker logs "$DIND" --tail 40 || true
        fatal 'inner dockerd not ready within 90s'
    fi
    sleep 2
done
nl "dind $DIND ready (engine $(docker exec "$DIND" docker version --format '{{.Server.Version}}' 2>/dev/null))"

nl 'pre-pulling fixture images (pinned digests)'
for img in "$VM_IMG" "$NODE_EXPORTER_IMG" "$CADVISOR_IMG" "$ALPINE_IMG"; do
    docker exec "$DIND" docker pull -q "$img" >/dev/null || fatal "pull $img"
done

nl 'staging binaries + config (exec+stdin)'
docker exec "$DIND" mkdir -p /opt/fleetly/bin /opt/fleetly/etc /var/lib/fleetly || fatal 'mkdir stage'
stage "$DIND" "$SCL_BIN_DIR/fleetlyd" /opt/fleetly/bin/fleetlyd
stage "$DIND" "$SCL_BIN_DIR/fleetly" /opt/fleetly/bin/fleetly
docker exec "$DIND" chmod +x /opt/fleetly/bin/fleetlyd /opt/fleetly/bin/fleetly || fatal 'chmod'

# fleetlyd 配置：离线 dind 形态——ACME/git 关闭、base_domain 留空。
cat >"$TMP/config.yaml" <<'EOF'
addr: "0.0.0.0:8420"
grpc:
  addr: "127.0.0.1:8421"
state:
  db_path: "/var/lib/fleetly/fleetly.db"
secrets:
  key_path: "/var/lib/fleetly/fleetly.key"
ingress:
  token_file: "/var/lib/fleetly/fleetly-ingress.token"
  cert_dir: "/var/lib/fleetly/fleetly-certs"
  acme:
    enabled: false
engine:
  deploy_timeout_seconds: 300
  observe_seconds: 5
  replicas_below_seconds: 5
  poll_seconds: 1
  drift_interval_seconds: 3600
git:
  enabled: false
metrics:
  retention_days: 14
logging:
  level: info
EOF
stage "$DIND" "$TMP/config.yaml" /opt/fleetly/etc/config.yaml

docker exec "$DIND" docker swarm init --advertise-addr eth0 >/dev/null || fatal 'swarm init'

docker exec "$DIND" sh -c 'cd /var/lib/fleetly && nohup /opt/fleetly/bin/fleetlyd -c /opt/fleetly/etc/config.yaml > /tmp/scl-fleetlyd.log 2>&1 & echo $! > /var/run/fleetlyd.pid'
SCL_LIVE=0
i=0
while [ "$i" -lt 90 ]; do
    if msh 'wget -q -T 3 -O /dev/null http://127.0.0.1:8420/healthz/liveness' 2>/dev/null; then
        SCL_LIVE=1
        break
    fi
    i=$((i + 2))
    sleep 2
done
[ "$SCL_LIVE" -eq 1 ] || {
    msh 'tail -40 /tmp/scl-fleetlyd.log' || true
    fatal 'fleetlyd not live within 90s'
}
nl 'fleetlyd live (liveness 200)'
# ── v0.3 归属管道 fixture（metrics.sh 同源）：founder 注册 → machine 令牌
#（平台面 admin 等价——metrics/scaling 均为平台/资源写面）。
CURLER="$DIND-curl"
docker rm -f "$CURLER" >/dev/null 2>&1 || true
docker run -d --name "$CURLER" --network "$BR_NET" "$CURL_IMAGE" sleep 100000 >/dev/null ||
    fatal "docker run $CURLER"
docker exec "$CURLER" curl -s -o /dev/null "http://10.220.0.10:8420/healthz/liveness" ||
    fatal 'curl helper cannot reach the REST face'
docker exec "$CURLER" curl -s -c /tmp/jar -X POST "http://10.220.0.10:8420/v1/auth/register" \
    -H 'Content-Type: application/json' \
    -d '{"email":"founder@e2e.test","password":"founder-pass-1","display_name":"Founder"}' \
    >/dev/null || fatal 'founder register'
SCL_TOKEN=$(docker exec "$CURLER" curl -s -b /tmp/jar -X POST "http://10.220.0.10:8420/v1/tokens" \
    -H 'Content-Type: application/json' \
    -d '{"machine":true,"note":"e2e machine token","scopes":["admin"]}' | grep -oE '"token": ?"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "$SCL_TOKEN" ] || fatal 'machine token mint failed'
docker rm -f "$CURLER" >/dev/null 2>&1 || true
FOUNDER_TEAM=founder
FOUNDER_PRJ=default
FOUNDER_PROJECT="$FOUNDER_TEAM/$FOUNDER_PRJ"
nl "founder registered; machine token minted; project context '$FOUNDER_PROJECT'"

# ───────── SCL-A: metrics opt-in（扩缩的前置面）
nl '=== SCL-A: metrics stack opt-in ==='
mode_set_ok() {
    fcli metrics mode set on >/dev/null 2>&1
}
if poll_until 60 mode_set_ok; then
    assert "SCL-A1 METRICS_MODE_SET_ACCEPTED" 0
else
    fcli metrics mode set on || true
    assert "SCL-A1 METRICS_MODE_SET_ACCEPTED" 1 "metrics mode set on never accepted"
fi

all_running() {
    for svc in fleetly-cadvisor fleetly-node-exporter fleetly-victoriametrics; do
        msh "docker service ps $svc --format '{{.CurrentState}}' | grep -q '^Running'" 2>/dev/null || return 1
    done
    return 0
}
if poll_until 300 all_running; then
    assert "SCL-A2 METRICS_STACK_THREE_RUNNING" 0
else
    m docker service ls || true
    assert "SCL-A2 METRICS_STACK_THREE_RUNNING" 1 "managed metrics stack never fully ran within 300s"
fi

reporting_ok() {
    fcli metrics status 2>/dev/null | grep -q 'nodes_reporting: 1/1'
}
if poll_until 150 reporting_ok; then
    assert "SCL-A3 METRICS_REPORTING_1_OF_1" 0
else
    fcli metrics status || true
    assert "SCL-A3 METRICS_REPORTING_1_OF_1" 1 "VM never reported a converged scrape target"
fi

# ───────── SCL-B: 打压应用 + 带卷应用部署
nl '=== SCL-B: burn app (sha1sum x2, cpu limit) + stateful app deploy ==='
cat >"$TMP/burn-compose.yaml" <<EOF
name: $BURN_APP
services:
  $BURN_SVC:
    image: $ALPINE_IMG
    command: ["sh", "-c", "sha1sum /dev/zero"]
    deploy:
      replicas: 2
      resources:
        limits:
          cpus: "1.5"
EOF
stage "$DIND" "$TMP/burn-compose.yaml" /opt/fleetly/scl-burn.yaml
docker exec -d -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$SCL_TOKEN" -e FLEETLY_PROJECT="$FOUNDER_PROJECT" \
    "$DIND" sh -c '/opt/fleetly/bin/fleetly deploy --timeout 300s /opt/fleetly/scl-burn.yaml > /tmp/scl-burn-deploy.log 2>&1'
burn_deploy_ok() {
    fcli deployments list --json "$BURN_APP" 2>/dev/null | grep -q '"status": "succeeded"'
}
if poll_until 300 burn_deploy_ok; then
    assert "SCL-B1 BURN_APP_DEPLOY_SUCCEEDED" 0
else
    fcli deployments list --json "$BURN_APP" 2>/dev/null || true
    msh 'cat /tmp/scl-burn-deploy.log' || true
    assert "SCL-B1 BURN_APP_DEPLOY_SUCCEEDED" 1 "burn deployment never succeeded"
fi

cat >"$TMP/vault-compose.yaml" <<EOF
name: $VAULT_APP
services:
  $VAULT_SVC:
    image: $ALPINE_IMG
    command: ["sleep", "600"]
    volumes:
      - vlt:/srv
    deploy:
      replicas: 1
      resources:
        limits:
          cpus: "1.0"
volumes:
  vlt:
EOF
stage "$DIND" "$TMP/vault-compose.yaml" /opt/fleetly/scl-vault.yaml
docker exec -d -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$SCL_TOKEN" -e FLEETLY_PROJECT="$FOUNDER_PROJECT" \
    "$DIND" sh -c '/opt/fleetly/bin/fleetly deploy --timeout 300s /opt/fleetly/scl-vault.yaml > /tmp/scl-vault-deploy.log 2>&1'
vault_deploy_ok() {
    fcli deployments list --json "$VAULT_APP" 2>/dev/null | grep -q '"status": "succeeded"'
}
if poll_until 300 vault_deploy_ok; then
    assert "SCL-B2 STATEFUL_APP_DEPLOY_SUCCEEDED" 0
else
    fcli deployments list --json "$VAULT_APP" 2>/dev/null || true
    msh 'cat /tmp/scl-vault-deploy.log' || true
    assert "SCL-B2 STATEFUL_APP_DEPLOY_SUCCEEDED" 1 "stateful deployment never succeeded"
fi

# ───────── SCL-C: 策略落库与读面回放
nl '=== SCL-C: autoscaling policies ==='
policy_set_ok() {
    fcli apps scaling set --min 1 --max 4 --cpu 20 --mem 0 --cooldown 180 \
        "$BURN_APP" "$BURN_SVC" >/dev/null 2>&1
}
if poll_until 60 policy_set_ok; then
    assert "SCL-C1 BURN_POLICY_SET" 0
else
    fcli apps scaling set --min 1 --max 4 --cpu 20 --mem 0 --cooldown 180 "$BURN_APP" "$BURN_SVC" || true
    assert "SCL-C1 BURN_POLICY_SET" 1 "burn policy never accepted"
fi
vault_policy_set_ok() {
    fcli apps scaling set --min 1 --max 2 --cpu 20 --mem 0 --cooldown 180 \
        "$VAULT_APP" "$VAULT_SVC" >/dev/null 2>&1
}
if poll_until 60 vault_policy_set_ok; then
    assert "SCL-C2 VAULT_POLICY_SET" 0
else
    fcli apps scaling set --min 1 --max 2 --cpu 20 --mem 0 --cooldown 180 "$VAULT_APP" "$VAULT_SVC" || true
    assert "SCL-C2 VAULT_POLICY_SET" 1 "vault policy never accepted"
fi

policy_view_ok() {
    fcli apps scaling show "$BURN_APP" "$BURN_SVC" 2>/dev/null |
        grep -q 'min/max replicas: 1/4' &&
        fcli apps scaling show "$BURN_APP" "$BURN_SVC" 2>/dev/null |
        grep -q 'targets: cpu 20%, mem 0%' &&
        fcli apps scaling show "$BURN_APP" "$BURN_SVC" 2>/dev/null |
        grep -q 'cooldown: 180s'
}
if poll_until 30 policy_view_ok; then
    assert "SCL-C3 POLICY_SHOW_READBACK" 0
else
    fcli apps scaling show "$BURN_APP" "$BURN_SVC" || true
    assert "SCL-C3 POLICY_SHOW_READBACK" 1 "scaling show does not replay the stored policy"
fi

# ───────── SCL-D: 诚实无数据（真实打压负载在跑而归属标签缺席）
nl '=== SCL-D: honest no_data disclosure before synthetic injection ==='
if poll_until 150 events_grep scaling.no_data; then
    assert "SCL-D1 NO_DATA_DISCLOSED" 0
else
    msh 'grep -c scaling /tmp/scl-events.txt 2>/dev/null' || true
    assert "SCL-D1 NO_DATA_DISCLOSED" 1 "scaling.no_data never disclosed while the burn app runs unlabeled"
fi

# ───────── SCL-E: 扩容（合成序列 +3.0/s = 2×1.5 核打满；真链路评估）
# vault 同拍 +3.0/s（cur=1 → pct=300% → 反解夹逼 max2）——stateful 扩向
# 一并走通（SCL-G0）。
nl '=== SCL-E: scale up 2 -> 4 through the real evaluation chain ==='
stage_import_loop
start_import_loop 30 30 100000
scaled_up() {
    [ "$(burn_replicas)" = "4" ] && [ "$(vault_replicas)" = "2" ]
}
if poll_until 420 scaled_up; then
    assert "SCL-E1 REPLICAS_SCALED_UP_2_TO_4" 0
else
    nl "burn replicas at cap: $(burn_replicas)"
    m docker service ls || true
    assert "SCL-E1 REPLICAS_SCALED_UP_2_TO_4" 1 "replicas never reached 4 (want 2->4 via convergence evaluation)"
fi

events_json_snap
adjusted_up_ok() {
    events_json_has 'scaling.adjusted' &&
        events_json_has "\"service\":\"$BURN_SVC\"" &&
        events_json_has '"replicas_before":"2"' &&
        events_json_has '"replicas_after":"4"' &&
        events_json_has '"dimension":"cpu"'
}
if adjusted_up_ok; then
    assert "SCL-E2 ADJUSTED_EVENT_UP_PAYLOAD" 0
else
    grep 'scaling.adjusted' "$TMP/scl-events-flat.json" | tail -2 || true
    assert "SCL-E2 ADJUSTED_EVENT_UP_PAYLOAD" 1 "scaling.adjusted event never carried the 2->4 cpu payload"
fi

policy_stable_ok() {
    fcli apps scaling show "$BURN_APP" "$BURN_SVC" 2>/dev/null | grep -q 'min/max replicas: 1/4' &&
        fcli apps scaling show "$BURN_APP" "$BURN_SVC" 2>/dev/null | grep -q 'cooldown: 180s'
}
if poll_until 30 policy_stable_ok; then
    assert "SCL-E3 POLICY_PROJECTION_STABLE_AFTER_ADJUST" 0
else
    fcli apps scaling show "$BURN_APP" "$BURN_SVC" || true
    assert "SCL-E3 POLICY_PROJECTION_STABLE_AFTER_ADJUST" 1 "the policy row drifted after the replica override"
fi

# ───────── SCL-F: 冷却窗护栏 + 冷却过期缩容（低水位 +0.5/s → 缩带）
# vault 同拍换 +0.1/s（cur=2、限额 1.0 → pct=5%<14% 缩带）——缩向提案
# 恒真，SCL-G 验护栏拦截。
nl '=== SCL-F: cooldown guard, then scale down after the window ==='
stop_import_loop
start_import_loop 5 1 200000
nl 'synthetic series injection switched to the shrink band (burn +0.5/s, vault +0.1/s)'

# 冷却窗（180s 自调整时刻起）内的护栏：静态等 70s（≥2 个 30s 评估拍）后
# 断副本未动。此刻距 E1 调整 ≤ ~40s——70s 等待全部落在冷却窗内。
sleep 70
if [ "$(burn_replicas)" = "4" ]; then
    assert "SCL-F1 COOLDOWN_GUARD_NO_SHRINK" 0
else
    nl "burn replicas during cooldown: $(burn_replicas)"
    assert "SCL-F1 COOLDOWN_GUARD_NO_SHRINK" 1 "replicas moved inside the 180s cooldown window"
fi

scaled_down() {
    [ "$(burn_replicas)" = "2" ]
}
if poll_until 400 scaled_down; then
    assert "SCL-F2 SHRINK_AFTER_COOLDOWN_4_TO_2" 0
else
    nl "burn replicas after window: $(burn_replicas)"
    assert "SCL-F2 SHRINK_AFTER_COOLDOWN_4_TO_2" 1 "replicas never shrank proportionally (4->2) after the cooldown expired"
fi

events_json_snap
adjusted_down_ok() {
    events_json_has 'scaling.adjusted' &&
        events_json_has '"replicas_before":"4"' &&
        events_json_has '"replicas_after":"2"'
}
if adjusted_down_ok; then
    assert "SCL-F3 ADJUSTED_EVENT_DOWN_PAYLOAD" 0
else
    grep 'scaling.adjusted' "$TMP/scl-events-flat.json" | tail -2 || true
    assert "SCL-F3 ADJUSTED_EVENT_DOWN_PAYLOAD" 1 "scaling.adjusted event never carried the 4->2 payload"
fi

# ───────── SCL-G: stateful 只扩不缩护栏（缩带判据冷却过期后仍恒真）
nl '=== SCL-G: stateful scale-up-only guard ==='
if [ "$(vault_replicas)" = "2" ]; then
    assert "SCL-G0 STATEFUL_SCALED_UP_1_TO_2" 0
else
    nl "vault replicas: $(vault_replicas)"
    assert "SCL-G0 STATEFUL_SCALED_UP_1_TO_2" 1 "the stateful service never scaled up 1->2 on the high watermark"
fi
# 缩带低水位已持续到冷却过期（F2 的 burn 已缩回 1——同拍判据下 vault 的
# 2→1 提案必须被护栏拦住）。
if [ "$(vault_replicas)" = "2" ]; then
    assert "SCL-G1 STATEFUL_HOLD_AT_2_UNDER_LOW_LOAD" 0
else
    nl "vault replicas after cooldown: $(vault_replicas)"
    assert "SCL-G1 STATEFUL_HOLD_AT_2_UNDER_LOW_LOAD" 1 "the stateful service shrank under sustained low utilization"
fi
vault_down_lines=$(grep "scaling.adjusted" "$TMP/scl-events-flat.json" 2>/dev/null | grep -F "\"app\":\"$VAULT_APP\"" | grep -cF '"replicas_after":"1"')
if [ "${vault_down_lines:-0}" = "0" ]; then
    assert "SCL-G2 NO_DOWN_ADJUSTED_FOR_STATEFUL" 0
else
    grep "scaling.adjusted" "$TMP/scl-events-flat.json" | grep -F "\"app\":\"$VAULT_APP\"" | tail -2 || true
    assert "SCL-G2 NO_DOWN_ADJUSTED_FOR_STATEFUL" 1 "the stateful service produced a scaling.adjusted (down) event"
fi

# ───────── SCL-H: 策略移除
nl '=== SCL-H: policy removal ==='
rm_ok() {
    fcli apps scaling rm "$BURN_APP" "$BURN_SVC" >/dev/null 2>&1
}
if poll_until 30 rm_ok; then
    assert "SCL-H1a POLICY_REMOVED" 0
else
    fcli apps scaling rm "$BURN_APP" "$BURN_SVC" || true
    assert "SCL-H1a POLICY_REMOVED" 1 "scaling rm never accepted"
fi
if fcli apps scaling show "$BURN_APP" "$BURN_SVC" >/dev/null 2>&1; then
    assert "SCL-H1b SHOW_AFTER_RM_REJECTED" 1 "scaling show still resolves a removed policy"
else
    assert "SCL-H1b SHOW_AFTER_RM_REJECTED" 0
fi

# ───────── SCL-I: metrics off → 休眠披露（策略行仍在场：vault 的策略）
nl '=== SCL-I: dormant disclosure on metrics off ==='
unset_ok() {
    fcli metrics mode set unset >/dev/null 2>&1
}
poll_until 60 unset_ok || fatal 'metrics mode set unset never accepted'
if poll_until 150 events_grep scaling.dormant; then
    assert "SCL-I1 DORMANT_DISCLOSED_ON_METRICS_OFF" 0
else
    assert "SCL-I1 DORMANT_DISCLOSED_ON_METRICS_OFF" 1 "scaling.dormant never disclosed after metrics off"
fi
stop_import_loop

finish
