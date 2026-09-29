#!/bin/sh
# e2e/logs-victorialogs.sh — E6 W5-S1 VictoriaLogs 默认捆绑端到端（单节点
# dind 形态；设计 docs/design/2026-09-22-observability.md §2/§3.1 的真机
# 闭环，s3-rustfs.sh 同骨架——编排自足、镜像钉 digest）：
#
#   交叉编译 linux/amd64 fleetlyd+fleetly（或复用 LOGS_BIN_DIR）→ 宿主 bridge
#   上起一个特权 dind（私网 10.216.0.0/24，镜像钉 digest）→ swarm init +
#   fleetlyd 起服 → 全部断言经 dind 内的 docker/fleetly CLI/REST 驱动：
#
#   A1 收敛管理器收敛（缺省即部署——logs.backend 未设置 = victorialogs 生效，
#      V2-1 默认捆绑）：fleetly-victorialogs 服务 running + 事件
#      logs.victorialogs_deployed + `logs backend show`（deployed）。
#   A2 回环可达（D-W5-4 等价承载形态：host 网络任务 + -httpListenAddr
#      回环监听——见 internal/victorialogs/spec.go 头注记）：dind 内
#      wget http://127.0.0.1:9428/health。
#   A3 入湖可查：部署应用产生日志 → 批量器（2s/512 行 flush）入湖 →
#      SearchLogs（REST /v1/apps/<app>/logs/search）命中 marker。
#   A4 keyword 过滤：不命中关键词 → 200 且零行（诚实空结果）。
#   A5 注入负向：恶意 keyword（引号/管道/正则元字符）→ 200 非 5xx、
#      合法 JSON（用户输入不逃逸出字面量短语语义——不炸不越权）。
#   A6 降级 streak + 直播不受影响：scale VL=0 → 事件 logs.ingest_degraded
#      → `logs follow` 照常出行（ring/fan-out 不经过批量器）。
#   A7 backend 切换：set jsonl → 服务移除 + 数据卷保留 + removed 事件 +
#      show=removed → 复切 victorialogs → 服务再收敛（show=deployed）。
#
#   断言组 B（W5-S2，设计 §3.2/§3.3——访问日志采集归因 + 统一检索消费面；
#   插在 A8 与 A9 之间——VL 在位窗口内执行）：
#   B1 traefik 静态参数收敛（--accesslog=true --accesslog.format=json——
#      spec 漂移比对覆盖，存量部署自动收敛；断言带重试窗）。
#   B2 build 应用部署成功（scratch-free：alpine + busybox httpd 的
#      Dockerfile 构建；build 行经咽喉点接入批量器，source=build）。
#   B3 构建日志可检索（SearchLogs source=build 命中构建 marker）。
#   B4 入口路由就绪（fleetly.domains label → traefik 动态配置收敛）。
#   B5 访问应用 URL 产生真实 traefik 访问行 → SearchLogs source=access
#      命中且带 method/status/route/deployment_id 字段（RouterName 反解 +
#      部署归因的端到端闭环）。
#   B6 CLI `logs search --keyword` 命中（统一检索的 CLI 消费面）。
#   B7 CLI 空结果诚实（200 空行集 → "no matches" 提示行、exit 0——与后端
#      不可达的报错形态区分）。
#
# 断言风格与 e2e/s3-rustfs.sh 一致（VL-x: PASS/FAIL 行 + NL_FAIL 计数 +
# finish）。
# usage: e2e/logs-victorialogs.sh
# env:
#   LOGS_DIND_IMAGE  dind 镜像（默认 docker:29.8.1-dind，钉 digest 与 CI 一致）
#   LOGS_SKIP_BUILD  1 = 跳过交叉编译，改用 LOGS_BIN_DIR 下的现成二进制
#   LOGS_BIN_DIR     LOGS_SKIP_BUILD=1 时的二进制来源（需含 fleetlyd 与 fleetly）
#   LOGS_VERSION     注入的版本串（默认 v0.2.0-logs-e2e）
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

# ── 镜像钉 digest（T0-V2.3 供应链；台账 docs/runbooks/image-prepull.md
#    #14——与 Go 常量 internal/victorialogs/spec.go DefaultVictoriaLogsImage
#    同源，改动须三处同步）。
DIND_IMAGE="${LOGS_DIND_IMAGE:-docker:29.8.1-dind@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0}"
ALPINE_IMG='alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc'
VL_IMG='victoriametrics/victoria-logs:v1.52.0@sha256:47b820890d64c4575a2a0a46415dcd8a4fd59a0f1fcd6a377693d7aea639442e'
# curl helper（v0.3 fixture：REST 注册/铸 PAT 面——auth.sh 同源钉版）。
CURL_IMAGE='curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69'
# whoami：B4/B5 访问日志归因的 HTTP 服务应用（multinode-rehearsal.sh 同款
# 钉版；:80 直答——alpine busybox 无 httpd applet 的教训后选真服务镜像）。
WHOAMI_IMG='traefik/whoami:v1.10.4@sha256:02d8fe035f170f91cbb5e458a57f4cefab747436f8244a0eb2d66785fe5e565f'
LOGS_SKIP_BUILD="${LOGS_SKIP_BUILD:-0}"
LOGS_BIN_DIR="${LOGS_BIN_DIR:-}"
LOGS_VERSION="${LOGS_VERSION:-v0.2.0-logs-e2e}"

BR_NET=fleetly-vl-br
BR_SUBNET=10.216.0.0/24
DIND=fleetly-vl-e2e-dind
APP=vlapp
SVC=web

NL_FAIL=0
SUITE_DINDS=''
ACTIVE_NET=''

nl() { printf '[vl-e2e %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
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
# fcli <args...> — dind 内的 fleetly CLI（gRPC 面 + founder PAT；项目上下文
# 经 FLEETLY_PROJECT 显式注入——v0.3 归属管道）。
fcli() {
    docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$VL_TOKEN" \
        -e FLEETLY_PROJECT="$FOUNDER_PROJECT" \
        "$DIND" /opt/fleetly/bin/fleetly "$@"
}
# rest_search <query> — REST 面 SearchLogs（gateway GET；founder PAT）。
rest_search() {
    # $1 = 查询串；$2 = app 流选择器（可选，缺省 $APP）。W2-S4 起 SearchLogs
    # 强制按 app 收口（rbac-teams §4.2）——检索结果只含所选 app 的流，跨 app
    # 的行（构建日志归构建 app、访问日志归路由 app）必须选对各自的流。
    app=${2:-$APP}
    m wget -q -T 10 -O - --header="Authorization: Bearer $VL_TOKEN" \
        "http://127.0.0.1:8420/v1/apps/$app/logs/search$1" 2>/dev/null
}
# events_grep <pattern> — 现抓事件流快照（busybox timeout 掐断 follow 流）
# 并在快照中检索。watch 是长驻流，快照即「迄今全部事件」。
events_grep() {
    docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$VL_TOKEN" \
        "$DIND" sh -c 'timeout 4 /opt/fleetly/bin/fleetly events watch --since-seq 0 > /tmp/vl-events.txt 2>/dev/null; exit 0'
    m grep -q "$1" /tmp/vl-events.txt
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
# 文本脚本 CR 剥离（s3-rustfs.sh 同款：busybox ash 不执行 CRLF）。
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
# mktemp 可能返回 MSYS 虚拟路径（/tmp/…）——原生 go.exe 在 MSYS_NO_PATHCONV=1
# 下拿不到映射；经 cygpath -m 归一为 C:/… 混合形态（sh/go/docker 三方都认）。
TMP=$(posix_path "$(mktemp -d)")
case "$TMP" in
/*)
    if command -v cygpath >/dev/null 2>&1; then
        TMP=$(cygpath -m "$TMP") || fatal 'cygpath -m on tmpdir'
    fi
    ;;
esac

command -v docker >/dev/null 2>&1 || fatal 'docker not on PATH'
command -v go >/dev/null 2>&1 || LOGS_SKIP_BUILD=1

# ─────────────────────────────────────────────────────────────── 构建
if [ "$LOGS_SKIP_BUILD" != '1' ]; then
    nl "cross-compiling linux/amd64 fleetlyd+fleetly ($LOGS_VERSION)"
    (
        cd "$ROOT" &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$LOGS_VERSION" \
                -o "$TMP/fleetlyd" ./cmd/fleetlyd &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$LOGS_VERSION" \
                -o "$TMP/fleetly" ./cmd/fleetly
    ) || fatal 'go build failed'
    LOGS_BIN_DIR="$TMP"
else
    LOGS_BIN_DIR="${LOGS_BIN_DIR:?LOGS_SKIP_BUILD=1 requires LOGS_BIN_DIR}"
    nl "using prebuilt binaries from $LOGS_BIN_DIR"
fi
[ -f "$LOGS_BIN_DIR/fleetlyd" ] || fatal "fleetlyd missing in $LOGS_BIN_DIR"
[ -f "$LOGS_BIN_DIR/fleetly" ] || fatal "fleetly missing in $LOGS_BIN_DIR"

# ─────────────────────────────────────────────────────── dind 编排准备
leftovers=$(docker ps -aq --filter name=fleetly-vl- 2>/dev/null || true)
if [ -n "$leftovers" ]; then
    nl "WARN removing leftover vl-e2e containers: $leftovers"
    echo "$leftovers" | xargs docker rm -f >/dev/null 2>&1 || true
fi
docker network rm "$BR_NET" >/dev/null 2>&1 || true
docker network create -d bridge --subnet "$BR_SUBNET" "$BR_NET" >/dev/null || fatal "create $BR_NET"
ACTIVE_NET="$BR_NET"

docker rm -f "$DIND" >/dev/null 2>&1 || true
docker run -d --name "$DIND" --privileged --hostname mgr \
    --network "$BR_NET" --ip 10.216.0.10 "$DIND_IMAGE" >/dev/null ||
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

nl 'staging binaries + config (exec+stdin)'
docker exec "$DIND" mkdir -p /opt/fleetly/bin /opt/fleetly/etc /var/lib/fleetly || fatal 'mkdir stage'
stage "$DIND" "$LOGS_BIN_DIR/fleetlyd" /opt/fleetly/bin/fleetlyd
stage "$DIND" "$LOGS_BIN_DIR/fleetly" /opt/fleetly/bin/fleetly
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
  deploy_timeout_seconds: 300
  observe_seconds: 5
  replicas_below_seconds: 5
  poll_seconds: 1
  drift_interval_seconds: 3600
git:
  enabled: false
logging:
  level: info
EOF
stage "$DIND" "$TMP/config.yaml" /opt/fleetly/etc/config.yaml

nl 'pre-pulling fixture images (pinned digests)'
for img in "$ALPINE_IMG" "$VL_IMG" "$WHOAMI_IMG"; do
    docker exec "$DIND" docker pull -q "$img" >/dev/null || fatal "pull $img"
done

docker exec "$DIND" docker swarm init --advertise-addr eth0 >/dev/null || fatal 'swarm init'

docker exec "$DIND" sh -c 'cd /var/lib/fleetly && nohup /opt/fleetly/bin/fleetlyd -c /opt/fleetly/etc/config.yaml > /tmp/vl-fleetlyd.log 2>&1 & echo $! > /var/run/fleetlyd.pid'
VL_LIVE=0
i=0
while [ "$i" -lt 90 ]; do
    if m sh -c 'wget -q -T 3 -O /dev/null http://127.0.0.1:8420/healthz/liveness' 2>/dev/null; then
        VL_LIVE=1
        break
    fi
    i=$((i + 2))
    sleep 2
done
[ "$VL_LIVE" -eq 1 ] || {
    msh 'tail -40 /tmp/vl-fleetlyd.log' || true
    fatal 'fleetlyd not live within 90s'
}
VL_TOKEN=$(m sh -c 'cat /var/lib/fleetly/bootstrap-token') || fatal 'read bootstrap token'
[ -n "$VL_TOKEN" ] || fatal 'empty bootstrap token'
nl 'fleetlyd live (liveness 200), bootstrap token read'

# ── v0.3 归属管道 fixture（rbac-teams §2.1/§2.3/§3.4）：注册 founder（首
# 用户 = 平台管理员 + 个人队 + 默认项目 default）→ 会话自服务铸用户 PAT
#（admin scope）。首次部署经 FLEETLY_PROJECT=founder/default 显式携带项目
# 归属（fcli 统一 env 注入）；bootstrap token 弃用。curl helper 同 auth.sh。
CURLER="$DIND-curl"
docker rm -f "$CURLER" >/dev/null 2>&1 || true
docker run -d --name "$CURLER" --network "$BR_NET" "$CURL_IMAGE" sleep 100000 >/dev/null ||
    fatal "docker run $CURLER"
docker exec "$CURLER" curl -s -o /dev/null "http://10.216.0.10:8420/healthz/liveness" ||
    fatal 'curl helper cannot reach the REST face'
docker exec "$CURLER" curl -s -c /tmp/jar -X POST "http://10.216.0.10:8420/v1/auth/register" \
    -H 'Content-Type: application/json' \
    -d '{"email":"founder@e2e.test","password":"founder-pass-1","display_name":"Founder"}' \
    >/dev/null || fatal 'founder register'
# W2-S5 夹具修正：凭据改铸 **machine 令牌**（平台级凭据 = 资源面 admin
# 等价，rbac-teams §2.3；W2-S4 起平台管理员在资源面被 ResolvePermission
# 短路为只读——founder 的用户 PAT 已不能再承担部署/资源写）。
VL_TOKEN=$(docker exec "$CURLER" curl -s -b /tmp/jar -X POST "http://10.216.0.10:8420/v1/tokens" \
    -H 'Content-Type: application/json' \
    -d '{"machine":true,"note":"e2e machine token","scopes":["admin"]}' | grep -oE '"token": ?"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "$VL_TOKEN" ] || fatal 'machine token mint failed'
# curl helper 用毕即除（fixture 只承担注册与铸 token；避免钉住 bridge 网络影响后续套件）。
docker rm -f "$CURLER" >/dev/null 2>&1 || true
FOUNDER_TEAM=founder
FOUNDER_PRJ=default
FOUNDER_PROJECT="$FOUNDER_TEAM/$FOUNDER_PRJ"
nl 'founder registered (platform admin); machine token minted; project context '"$FOUNDER_PROJECT"

# ───────── A1: 收敛管理器收敛（缺省即部署——未显式设置 = victorialogs 生效）
nl '=== A1: manager converges on default (logs.backend unset -> victorialogs) ==='
vl_running() {
    msh "docker service ps fleetly-victorialogs --format '{{.CurrentState}}' | grep -q '^Running'" 2>/dev/null
}
if poll_until 240 vl_running; then
    assert "VL-A1 SERVICE_RUNNING_ON_DEFAULT" 0
else
    m docker service ps fleetly-victorialogs --no-trunc || true
    assert "VL-A1 SERVICE_RUNNING_ON_DEFAULT" 1 "fleetly-victorialogs never ran within 240s"
fi

if poll_until 60 events_grep logs.victorialogs_deployed; then
    assert "VL-A2 DEPLOYED_EVENT" 0
else
    assert "VL-A2 DEPLOYED_EVENT" 1 "logs.victorialogs_deployed missing from event stream"
fi

# 回环可达（VL 原生 /health；host 网络任务自绑 127.0.0.1——零公网面）。
vl_health_ok() {
    msh "wget -q -T 3 -O - http://127.0.0.1:9428/health | grep -q OK" 2>/dev/null
}
if poll_until 60 vl_health_ok; then
    assert "VL-A3 LOOPBACK_HEALTH_OK" 0
else
    msh 'wget -S -T 3 -O - http://127.0.0.1:9428/health' || true
    assert "VL-A3 LOOPBACK_HEALTH_OK" 1 "VL /health unreachable on 127.0.0.1:9428"
fi

# backend show：缺省模式 + 部署在位（收敛管理器已收敛）。
backend_show_ok() {
    fcli logs backend show 2>/dev/null |
        grep -q 'backend: victorialogs' &&
        fcli logs backend show 2>/dev/null | grep -q 'deployment: deployed'
}
if poll_until 60 backend_show_ok; then
    assert "VL-A4 BACKEND_SHOW_DEPLOYED" 0
else
    fcli logs backend show || true
    assert "VL-A4 BACKEND_SHOW_DEPLOYED" 1 "logs backend show not deployed/victorialogs"
fi

# ───────── A3: 入湖可查（部署应用 → 采集 → 批量器 → SearchLogs 命中）
nl '=== A5: app logs flow into the store and SearchLogs hits the marker ==='
MARKER1="vlmarker-$(date +%s)-alpha"
MARKER2="vlmarker-$(date +%s)-bravo"
cat >"$TMP/app-compose.yaml" <<EOF
name: $APP
services:
  $SVC:
    image: $ALPINE_IMG
    command: ["sh", "-c", "while true; do echo boot-$MARKER1; echo tick-$MARKER2; sleep 1; done"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 2s
      timeout: 1s
      retries: 100
      start_period: 0s
EOF
stage "$DIND" "$TMP/app-compose.yaml" /opt/fleetly/vlapp-compose.yaml

docker exec -d -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$VL_TOKEN" -e FLEETLY_PROJECT="$FOUNDER_PROJECT" \
    "$DIND" sh -c '/opt/fleetly/bin/fleetly deploy --timeout 300s /opt/fleetly/vlapp-compose.yaml > /tmp/vl-deploy.log 2>&1'
deploy_succeeded() {
    fcli deployments list --json "$APP" 2>/dev/null | grep -q '"status": "succeeded"'
}
if poll_until 300 deploy_succeeded; then
    assert "VL-A5 APP_DEPLOY_SUCCEEDED" 0
else
    fcli deployments list --json "$APP" 2>/dev/null || true
    msh 'cat /tmp/vl-deploy.log' || true
    assert "VL-A5 APP_DEPLOY_SUCCEEDED" 1 "deployment never succeeded (status unknown)"
fi

# 采集（2s 轮询）→ 脱敏 → 批量器（2s flush）→ VL 索引，检索面以
# SearchLogs 命中 marker 为准（REST gateway GET；bootstrap token）。
search_hit() {
    rest_search "?keyword=$MARKER1" | grep -q "$MARKER1"
}
if poll_until 120 search_hit; then
    assert "VL-A6 SEARCHLOGS_HIT_MARKER" 0
else
    rest_search "?keyword=$MARKER1" || true
    assert "VL-A6 SEARCHLOGS_HIT_MARKER" 1 "marker never searchable within 120s"
fi

# ───────── A4: keyword 过滤（不命中 = 200 空结果，非错误）
# gateway 对空 repeated 字段不输出（EmitUnpopulated=false）→ 空结果体 =
# `{}`（或带空 rows 数组）。
nl '=== A7: keyword filter (honest empty result on 200) ==='
EMPTY=$(rest_search "?keyword=nomatch-nothing-$(date +%s)" 2>/dev/null)
case "$EMPTY" in
'{}' | *'"rows": []'* | *'"rows":[]'*) assert "VL-A7 SEARCH_EMPTY_HONEST_200" 0 ;;
"") fail "VL-A7 SEARCH_EMPTY_HONEST_200" "empty response body (expected 200 JSON)" ;;
*) assert "VL-A7 SEARCH_EMPTY_HONEST_200" 0 "note: body=$EMPTY" ;;
esac

# ───────── A5: 注入负向（恶意 keyword 不逃逸、不 5xx）
# 恶意串（URL 编码）：evil" or app=~"^(.*)$" | wrap —— 引号闭合/正则/管道
# 全开；唯一合法结局 = 200 + 合法 JSON（短语字面语义吞掉输入）。busybox
# wget 非 200 即非零退出 → 空 body → fail。
nl '=== A8: injection negative (malicious keyword stays a literal phrase) ==='
EVIL_ENC='evil%22%20or%20app%3D~%22%5E(.*)%24%22%20%7C%20wrap'
EVIL_BODY=$(rest_search "?keyword=$EVIL_ENC" 2>/dev/null)
case "$EVIL_BODY" in
*'"rows"'* | '{}' | *'"rows": []'*) assert "VL-A8 EVIL_KEYWORD_NOT_5XX" 0 ;;
"") fail "VL-A8 EVIL_KEYWORD_NOT_5XX" "no 200 body for the malicious keyword (5xx or transport failure)" ;;
*) assert "VL-A8 EVIL_KEYWORD_NOT_5XX" 0 "note: body=$EVIL_BODY" ;;
esac

# ───────── 断言组 B（W5-S2）：访问日志采集归因 + 统一检索消费面 ─────────
BUILD_APP=vlbapp
BUILD_MARKER="vlbuild-$(date +%s)"

nl '=== B1: traefik accesslog args converged (spec drift comparison covers --accesslog) ==='
accesslog_args_ok() {
    args=$(m docker service inspect fleetly-ingress --format '{{.Spec.TaskTemplate.ContainerSpec.Args}}' 2>/dev/null)
    printf '%s' "$args" | grep -q -- '--accesslog=true' &&
        printf '%s' "$args" | grep -q -- '--accesslog.format=json'
}
if poll_until 120 accesslog_args_ok; then
    assert "VL-B1 INGRESS_ACCESSLOG_ARGS_CONVERGED" 0
else
    m docker service inspect fleetly-ingress --format '{{.Spec.TaskTemplate.ContainerSpec.Args}}' || true
    assert "VL-B1 INGRESS_ACCESSLOG_ARGS_CONVERGED" 1 "accesslog args never converged within 120s"
fi

# B2 素材：Dockerfile 构建应用（alpine——零外部载荷；RUN 指令行本身即构建
# 日志 marker，buildkit 逐行 tee 进批量器，source=build）。运行面 = echo 循环
# （构建应用只需要「活着」——B4/B5 的访问日志归因打 whoami 预构建应用，
# alpine busybox 无 httpd applet 的教训不重演）。素材目录必须在 dind 的 /tmp
# 下——H14 执行侧上下文守卫：build context 只能住在平台受管根（/tmp、git
# root）内；conformance-builder 在 runner 本机 /tmp 跑同因。/opt 下的第一
# 次尝试曾瞬时 E_BUILD_FAILED（W5-S2 门上取证：context_dir outside managed
# roots）。
nl '=== B2: build app deploys (build lines flow into the store, source=build) ==='
msh "mkdir -p /tmp/$BUILD_APP/src" || fatal "mkdir $BUILD_APP"
cat >"$TMP/vlb-Dockerfile" <<EOF
FROM $ALPINE_IMG
RUN mkdir -p /www && echo $BUILD_MARKER > /www/index.html
CMD ["sh", "-c", "while true; do echo built-app-running; sleep 5; done"]
EOF
cat >"$TMP/vlb-compose.yaml" <<EOF
name: $BUILD_APP
services:
  web:
    build:
      context: src
      dockerfile: Dockerfile
    healthcheck:
      test: ["CMD", "true"]
      interval: 2s
      timeout: 1s
      retries: 100
      start_period: 0s
EOF
stage "$DIND" "$TMP/vlb-Dockerfile" "/tmp/$BUILD_APP/src/Dockerfile"
stage "$DIND" "$TMP/vlb-compose.yaml" "/tmp/$BUILD_APP/compose.yaml"

# buildkitd 预热门（平台 lazy 预热 buildkit 代理——首个构建须等其 Running；
# conformance-builder.sh 同款纪律。超预算也继续——构建 ensure 幂等自建）。
buildkit_ready() {
    msh "docker inspect -f '{{.State.Running}}' fleetly-buildkit 2>/dev/null" | grep -q true
}
poll_until 480 buildkit_ready || nl "WARN buildkitd warmup over budget; attempting the build anyway (ensure path self-builds)"

# 产品语义（engine 构建直通核对，engine.go「run fleetly build before
# deploying」）：deploy 不自动构建——先 `fleetly build` 逐服务入队并等待
# 终态（conformance-builder.sh CB-A 同款次序）。构建行经咽喉点入湖 = B3
# 的输入面；构建成功后 deploy 直通核对接 spec_hash 命中。
nl '  building the image first (fleetly build, then deploy)'
docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$VL_TOKEN" -e FLEETLY_PROJECT="$FOUNDER_PROJECT" \
    "$DIND" /opt/fleetly/bin/fleetly build --timeout 10m --json \
    "/tmp/$BUILD_APP/compose.yaml" >/dev/null 2>&1
build_succeeded() {
    fcli builds list --json "$BUILD_APP" 2>/dev/null | grep -q '"status": "succeeded"'
}
if poll_until 600 build_succeeded; then
    assert "VL-B2a BUILD_SUCCEEDED" 0
else
    fcli builds list --json "$BUILD_APP" 2>/dev/null || true
    assert "VL-B2a BUILD_SUCCEEDED" 1 "fleetly build never reached succeeded within 600s"
fi

docker exec -d -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$VL_TOKEN" -e FLEETLY_PROJECT="$FOUNDER_PROJECT" \
    "$DIND" sh -c "/opt/fleetly/bin/fleetly deploy --timeout 600s /tmp/$BUILD_APP/compose.yaml > /tmp/vl-deploy-build.log 2>&1"
build_deploy_succeeded() {
    fcli deployments list --json "$BUILD_APP" 2>/dev/null | grep -q '"status": "succeeded"'
}
if poll_until 600 build_deploy_succeeded; then
    assert "VL-B2 BUILD_APP_DEPLOY_SUCCEEDED" 0
else
    fcli deployments list --json "$BUILD_APP" 2>/dev/null || true
    msh 'cat /tmp/vl-deploy-build.log' || true
    assert "VL-B2 BUILD_APP_DEPLOY_SUCCEEDED" 1 "build deployment never succeeded"
fi

# B3 构建日志可检索（buildkit 逐行 tee → 批量器 2s flush → VL 索引）。
# W2-S4 起 SearchLogs 按 app 收口——构建行归构建 app（vlbapp），检索选它的流。
nl '=== B3: build logs are searchable (source=build hits the marker) ==='
build_search_hit() {
    rest_search "?keyword=$BUILD_MARKER&sources=build" "$BUILD_APP" | grep -q "$BUILD_MARKER"
}
if poll_until 120 build_search_hit; then
    assert "VL-B3 BUILD_LOG_SEARCHABLE" 0
else
    rest_search "?keyword=$BUILD_MARKER&sources=build" "$BUILD_APP" || true
    assert "VL-B3 BUILD_LOG_SEARCHABLE" 1 "build marker never searchable within 120s"
fi

# B4 入口路由就绪（fleetly.domains label → 动态配置收敛；traefik 任务重建
# ——attach 网络——与 pollInterval 都在重试窗内）。HTTP 面载体 = whoami
# 预构建应用（B2 构建应用不服务 HTTP——alpine busybox 无 httpd applet）。
nl '=== B4: ingress route ready for the access-log app (whoami) ==='
WEB_APP=vlweb
WEB_DOMAIN=vlweb.test.local
cat >"$TMP/vlweb-compose.yaml" <<EOF
name: $WEB_APP
services:
  web:
    image: $WHOAMI_IMG
    expose: ["80"]
    healthcheck:
      test: ["NONE"]
    labels:
      fleetly.domains: "$WEB_DOMAIN"
EOF
stage "$DIND" "$TMP/vlweb-compose.yaml" /opt/fleetly/vlweb-compose.yaml
docker exec -d -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$VL_TOKEN" -e FLEETLY_PROJECT="$FOUNDER_PROJECT" \
    "$DIND" sh -c '/opt/fleetly/bin/fleetly deploy --timeout 300s /opt/fleetly/vlweb-compose.yaml > /tmp/vl-deploy-web.log 2>&1'
web_deploy_succeeded() {
    fcli deployments list --json "$WEB_APP" 2>/dev/null | grep -q '"status": "succeeded"'
}
if poll_until 300 web_deploy_succeeded; then
    assert "VL-B4a WEB_APP_DEPLOY_SUCCEEDED" 0
else
    fcli deployments list --json "$WEB_APP" 2>/dev/null || true
    msh 'cat /tmp/vl-deploy-web.log' || true
    assert "VL-B4a WEB_APP_DEPLOY_SUCCEEDED" 1 "whoami deployment never succeeded"
fi
ACCESS_MARKER="vlbaccess-$(date +%s)"
route_ready() {
    m wget -q -T 5 -O /dev/null --header "Host: $WEB_DOMAIN" \
        "http://127.0.0.1/$ACCESS_MARKER?q=1" 2>/dev/null
}
if poll_until 120 route_ready; then
    assert "VL-B4 INGRESS_ROUTE_READY" 0
else
    m docker service ls || true
    assert "VL-B4 INGRESS_ROUTE_READY" 1 "route for $WEB_DOMAIN never answered within 120s"
fi

# B5 访问日志端到端：真实 traefik 访问行（JSON 含 RouterName）→ hub 反解
# app/service（ingress 命名公式候选匹配）→ 部署归因 → SearchLogs
# source=access 命中且带 method/status/route/deployment_id 字段。采集轮询
# （2s）+ flush（2s）+ VL 索引都在重试窗内。
nl '=== B5: access log collected and attributable (source=access with fields) ==='
m wget -q -T 5 -O /dev/null --header "Host: $WEB_DOMAIN" \
    "http://127.0.0.1/$ACCESS_MARKER?q=1" 2>/dev/null || true
access_search_hit() {
    # W2-S4 起 SearchLogs 按 app 收口——访问行归路由 app（vlweb），选它的流。
    body=$(rest_search "?keyword=$ACCESS_MARKER&sources=access" "$WEB_APP")
    printf '%s' "$body" | grep -q "$ACCESS_MARKER" &&
        printf '%s' "$body" | grep -q '"method"' &&
        printf '%s' "$body" | grep -q '"status"' &&
        printf '%s' "$body" | grep -q '"route"' &&
        printf '%s' "$body" | grep -q '"deployment_id"'
}
if poll_until 120 access_search_hit; then
    assert "VL-B5 ACCESS_LOG_SEARCHABLE_WITH_FIELDS" 0
else
    rest_search "?keyword=$ACCESS_MARKER&sources=access" "$WEB_APP" || true
    assert "VL-B5 ACCESS_LOG_SEARCHABLE_WITH_FIELDS" 1 "access row with fields never searchable within 120s"
fi

# B6 CLI 统一检索消费面：`logs search --keyword` 命中（A5 应用的容器行；
# 旗标在位置参数前——本仓 CLI 约定）。
nl '=== B6: CLI logs search hits the container marker ==='
cli_search_hit() {
    fcli logs search --keyword "$MARKER2" --source container --limit 5 "$FOUNDER_PROJECT/$APP" 2>/dev/null |
        grep -q "$MARKER2"
}
if poll_until 60 cli_search_hit; then
    assert "VL-B6 CLI_SEARCH_KEYWORD_HIT" 0
else
    fcli logs search --keyword "$MARKER2" --source container --limit 5 "$FOUNDER_PROJECT/$APP" || true
    assert "VL-B6 CLI_SEARCH_KEYWORD_HIT" 1 "CLI search never hit the container marker"
fi

# B7 CLI 空结果诚实：不命中关键词 = exit 0 + "no matches" 提示行（与后端
# 不可达的报错退出形态区分——设计 §3.1 诚实边界）。
nl '=== B7: CLI empty result is honest (exit 0 with a no-matches line) ==='
CLI_EMPTY=$(fcli logs search --keyword "no-such-$(date +%s)" --limit 5 "$APP" 2>/dev/null)
CLI_EMPTY_RC=$?
if [ "$CLI_EMPTY_RC" -eq 0 ] && printf '%s' "$CLI_EMPTY" | grep -q 'no matches'; then
    assert "VL-B7 CLI_SEARCH_EMPTY_HONEST" 0
else
    assert "VL-B7 CLI_SEARCH_EMPTY_HONEST" 1 "rc=$CLI_EMPTY_RC body=$CLI_EMPTY"
fi

# ───────── A6: 降级 streak + 直播不受影响（A3 条款）
nl '=== A9: degraded streak on VL down; live tail unaffected ==='
m docker service scale fleetly-victorialogs=0 >/dev/null || fatal 'scale VL to 0'
if poll_until 120 events_grep logs.ingest_degraded; then
    assert "VL-A9 INGEST_DEGRADED_EVENT" 0
else
    assert "VL-A9 INGEST_DEGRADED_EVENT" 1 "logs.ingest_degraded missing within 120s"
fi

# 直播面零影响：Follow 从 ring 回放/实时扇出照常（不经过批量器）。
follow_lines=$(docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$VL_TOKEN" \
    "$DIND" sh -c "timeout 8 /opt/fleetly/bin/fleetly logs follow --service $SVC $APP 2>/dev/null; exit 0" |
    grep -c "$MARKER2" || true)
[ "${follow_lines:-0}" -ge 1 ]
assert "VL-A10 FOLLOW_UNAFFECTED_WHILE_DEGRADED" $? "follow captured $follow_lines marker lines while VL was down"

# ───────── A7: backend 切换（jsonl：服务移除、卷保留；复切再收敛）
nl '=== A11: backend switch jsonl (service removed, volume retained) ==='
fcli logs backend set jsonl >/dev/null 2>&1 || fatal 'logs backend set jsonl'
vl_gone() {
    msh "docker service ls --format '{{.Name}}' | grep -q '^fleetly-victorialogs$'" 2>/dev/null
    [ $? -ne 0 ]
}
if poll_until 120 vl_gone; then
    assert "VL-A11 SERVICE_REMOVED_ON_JSONL" 0
else
    m docker service ls || true
    assert "VL-A11 SERVICE_REMOVED_ON_JSONL" 1 "service still present after set jsonl"
fi

vol_kept() {
    msh "docker volume ls -q | grep -q '^fleetly-victorialogs-data$'" 2>/dev/null
}
if poll_until 30 vol_kept; then
    assert "VL-A12 VOLUME_RETAINED_ON_JSONL" 0
else
    m docker volume ls || true
    assert "VL-A12 VOLUME_RETAINED_ON_JSONL" 1 "fleetly-victorialogs-data must survive the switch"
fi

if poll_until 60 events_grep logs.victorialogs_removed; then
    assert "VL-A13 REMOVED_EVENT" 0
else
    assert "VL-A13 REMOVED_EVENT" 1 "logs.victorialogs_removed missing from event stream"
fi

show_removed_ok() {
    fcli logs backend show 2>/dev/null |
        grep -q 'backend: jsonl' &&
        fcli logs backend show 2>/dev/null | grep -q 'deployment: removed'
}
if poll_until 30 show_removed_ok; then
    assert "VL-A14 BACKEND_SHOW_JSONL_REMOVED" 0
else
    fcli logs backend show || true
    assert "VL-A14 BACKEND_SHOW_JSONL_REMOVED" 1 "logs backend show should read jsonl/removed"
fi

# 复切 victorialogs：服务再收敛（卷复用——数据存续语义）。
nl '=== A15: switch back to victorialogs (service re-converges) ==='
fcli logs backend set victorialogs >/dev/null 2>&1 || fatal 'logs backend set victorialogs'
show_back_ok() {
    fcli logs backend show 2>/dev/null | grep -q 'deployment: deployed'
}
if poll_until 240 vl_running; then
    assert "VL-A15 SERVICE_RECONVERGED" 0
else
    m docker service ps fleetly-victorialogs --no-trunc || true
    assert "VL-A15 SERVICE_RECONVERGED" 1 "service never re-converged after switching back"
fi
if poll_until 60 show_back_ok; then
    assert "VL-A16 BACKEND_SHOW_DEPLOYED_AGAIN" 0
else
    fcli logs backend show || true
    assert "VL-A16 BACKEND_SHOW_DEPLOYED_AGAIN" 1 "logs backend show should read deployed again"
fi
if vol_kept; then
    assert "VL-A17 VOLUME_STILL_RETAINED" 0
else
    assert "VL-A17 VOLUME_STILL_RETAINED" 1 "volume must persist across the round trip"
fi

finish
