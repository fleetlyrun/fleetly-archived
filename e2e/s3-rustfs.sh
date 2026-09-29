#!/bin/sh
# e2e/s3-rustfs.sh — E3-4/E3-5 对象存储端到端（单节点 dind 形态；设计
# docs/design/2026-09-20-object-storage.md §2.4/§2.5 的真机闭环）：
#
# 编排（宿主侧，自足；multinode-rehearsal.sh 同骨架）：
#   交叉编译 linux/amd64 fleetlyd+fleetly（或复用 S3_BIN_DIR）→ 宿主 bridge
#   上起一个特权 dind（私网 10.215.0.0/24，镜像钉 digest）→ swarm init +
#   fleetlyd 起服 → 全部断言经 dind 内的 docker/fleetly CLI 驱动：
#
#   A0 前置校验（E_S3_NOT_CONFIGURED，诚实拒绝）：s3.mode=unset 时部署带
#      fleetly.s3=true 的 compose → 部署失败且错误含 E_S3_NOT_CONFIGURED。
#   A1 RustFS 收敛管理器收敛（E3-5）：s3 set --mode rustfs → fleetly-rustfs 服务
#      running（钉版镜像/内部网络/零 host 端口由 spec 构造单测钉死，此处
#      黑盒断言收敛）→ s3.rustfs_deployed 事件落库 → `fleetly s3 test`
#      探针通过（put→get→delete 真实往返 = 平台托管凭据 + 桶就绪）。
#   A2 注入与牵线（E3-4）：带 fleetly.s3=true 的最小应用部署成功 → 容器内
#      六键 env 注入断言（endpoint/bucket/path-style 取派生值、AK 非空）→
#      经注入端点用注入凭据做真实 S3 读写（restic 一次性容器挂
#      fleetly-rustfs-net：init/backup/dump 往返 marker 字节）。
#   A3 上传轨（E3-3 接 E3-5）：fleetly backups create → upload_status=ok
#     （restic 上传容器走 fleetly-rustfs-net + 托管凭据 + 回读校验）。
#   A4 禁用语义（D-S3-7）：s3 set --mode unset → 服务移除、**数据卷保留**。
#   A5 状态面（D-S3-8 诚实口径）：fleetly s3 status 明示便捷层非灾备文案。
#
# 断言风格与 e2e/nightly 一致（S3-x: PASS/FAIL 行 + NL_FAIL 计数 + finish）。
# usage: e2e/s3-rustfs.sh
# env:
#   S3_DIND_IMAGE  dind 镜像（默认 docker:29.8.1-dind，钉 digest 与 CI 一致）
#   S3_SKIP_BUILD  1 = 跳过交叉编译，改用 S3_BIN_DIR 下的现成二进制
#   S3_BIN_DIR     S3_SKIP_BUILD=1 时的二进制来源（需含 fleetlyd 与 fleetly）
#   S3_VERSION     注入的版本串（默认 v0.2.0-s3-e2e）
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

# ── 镜像钉 digest（T0-V2.3 供应链；台账见 docs/runbooks/image-prepull.md）。
DIND_IMAGE="${S3_DIND_IMAGE:-docker:29.8.1-dind@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0}"
CURL_IMAGE='curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69'
ALPINE_IMG='alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc'
RUSTFS_IMG='rustfs/rustfs:1.0.0@sha256:8cc9801755448b71a786705ce76692c77e14936cccd87cf2fc31842e58f4d1ff'
RESTIC_IMG='restic/restic:0.19.1@sha256:136600b6ff6843d61d355f7f71f460a166429f35de6fd11b568fece3c9a4d510'
S3_SKIP_BUILD="${S3_SKIP_BUILD:-0}"
S3_BIN_DIR="${S3_BIN_DIR:-}"
S3_VERSION="${S3_VERSION:-v0.2.0-s3-e2e}"

BR_NET=fleetly-s3-br
BR_SUBNET=10.215.0.0/24
DIND=fleetly-s3-dind
APP=s3app
SVC=web
APP_VOL=s3e2e-data
APP_NET=fleetly-$APP-net

NL_FAIL=0
SUITE_DINDS=''
ACTIVE_NET=''

nl() { printf '[s3-e2e %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
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
# fcli <args...> — dind 内的 fleetly CLI（gRPC 面 + bootstrap token）。
fcli() {
    docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_PROJECT="$FOUNDER_PROJECT" -e FLEETLY_TOKEN="$S3_TOKEN" \
        "$DIND" /opt/fleetly/bin/fleetly "$@"
}
# events_grep <pattern> — 现抓事件流快照（busybox timeout 掐断 follow 流）
# 并在快照中检索。watch 是长驻流，快照即「迄今全部事件」。
events_grep() {
    docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$S3_TOKEN" \
        "$DIND" sh -c 'timeout 4 /opt/fleetly/bin/fleetly events watch --since-seq 0 > /tmp/s3-events.txt 2>/dev/null; exit 0'
    m grep -q "$1" /tmp/s3-events.txt
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
    exit "$rc"
}
trap on_exit EXIT INT TERM

# stage <dind> <local-file> <remote-path> — exec+stdin 直传 + 两侧体积校验 +
# 文本脚本 CR 剥离（run.sh 同款：busybox ash 不执行 CRLF；Windows 检出风险面）。
stage() {
    d=$1
    f=$2
    r=$3
    docker exec -i "$d" sh -c "cat > '$r'" <"$f" || fatal "staging $r"
    hsz=$(wc -c <"$f" | tr -d ' ')
    gsz=$(docker exec "$d" sh -c "wc -c < '$r'" | tr -d ' ')
    [ "$hsz" = "$gsz" ] || fatal "size mismatch for $r: host=$hsz dind=$gsz"
    case "$r" in
    *.sh | *.yaml | *.yml)
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
command -v go >/dev/null 2>&1 || S3_SKIP_BUILD=1

# ─────────────────────────────────────────────────────────────── 构建
if [ "$S3_SKIP_BUILD" != '1' ]; then
    nl "cross-compiling linux/amd64 fleetlyd+fleetly ($S3_VERSION)"
    (
        cd "$ROOT" &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$S3_VERSION" \
                -o "$TMP/fleetlyd" ./cmd/fleetlyd &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$S3_VERSION" \
                -o "$TMP/fleetly" ./cmd/fleetly
    ) || fatal 'go build failed'
    S3_BIN_DIR="$TMP"
else
    S3_BIN_DIR="${S3_BIN_DIR:?S3_SKIP_BUILD=1 requires S3_BIN_DIR}"
    nl "using prebuilt binaries from $S3_BIN_DIR"
fi
[ -f "$S3_BIN_DIR/fleetlyd" ] || fatal "fleetlyd missing in $S3_BIN_DIR"
[ -f "$S3_BIN_DIR/fleetly" ] || fatal "fleetly missing in $S3_BIN_DIR"

# ─────────────────────────────────────────────────────── dind 编排准备
# 防御性清扫：上次崩溃残留会污染断言。
leftovers=$(docker ps -aq --filter name=fleetly-s3- 2>/dev/null || true)
if [ -n "$leftovers" ]; then
    nl "WARN removing leftover s3-e2e containers: $leftovers"
    echo "$leftovers" | xargs docker rm -f >/dev/null 2>&1 || true
fi
docker network rm "$BR_NET" >/dev/null 2>&1 || true
docker network create -d bridge --subnet "$BR_SUBNET" "$BR_NET" >/dev/null || fatal "create $BR_NET"
ACTIVE_NET="$BR_NET"

docker rm -f "$DIND" >/dev/null 2>&1 || true
docker run -d --name "$DIND" --privileged --hostname mgr \
    --network "$BR_NET" --ip 10.215.0.10 "$DIND_IMAGE" >/dev/null ||
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
stage "$DIND" "$S3_BIN_DIR/fleetlyd" /opt/fleetly/bin/fleetlyd
stage "$DIND" "$S3_BIN_DIR/fleetly" /opt/fleetly/bin/fleetly
docker exec "$DIND" chmod +x /opt/fleetly/bin/fleetlyd /opt/fleetly/bin/fleetly || fatal 'chmod'

# fleetlyd 配置：离线 dind 形态——ACME/git 关闭、base_domain 留空
#（s3.public_exposed 的公网面属 E3-6/S4 票，本票不涉）。
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
for img in "$ALPINE_IMG" "$RUSTFS_IMG" "$RESTIC_IMG"; do
    docker exec "$DIND" docker pull -q "$img" >/dev/null || fatal "pull $img"
done

docker exec "$DIND" docker swarm init --advertise-addr eth0 >/dev/null || fatal 'swarm init'

docker exec "$DIND" sh -c 'cd /var/lib/fleetly && nohup /opt/fleetly/bin/fleetlyd -c /opt/fleetly/etc/config.yaml > /tmp/s3-fleetlyd.log 2>&1 & echo $! > /var/run/fleetlyd.pid'
S3_LIVE=0
i=0
while [ "$i" -lt 90 ]; do
    if m sh -c 'wget -q -T 3 -O /dev/null http://127.0.0.1:8420/healthz/liveness' 2>/dev/null; then
        S3_LIVE=1
        break
    fi
    i=$((i + 2))
    sleep 2
done
[ "$S3_LIVE" -eq 1 ] || {
    msh 'tail -40 /tmp/s3-fleetlyd.log' || true
    fatal 'fleetlyd not live within 90s'
}
S3_TOKEN=$(m sh -c 'cat /var/lib/fleetly/bootstrap-token') || fatal 'read bootstrap token'
[ -n "$S3_TOKEN" ] || fatal 'empty bootstrap token'
nl 'fleetlyd live (liveness 200), bootstrap token read'
# ── v0.3 归属管道 fixture（rbac-teams §2.1/§2.3/§3.4）：注册 founder（首
# 用户 = 平台管理员 + 个人队 + 默认项目 default）→ 会话自服务铸用户 PAT
#（admin scope——founder 是平台管理员可达集；CLI 不消费会话 cookie）。
# 首次部署/建库经 FLEETLY_PROJECT=founder/default 显式携带项目归属；fcli
# 统一 env 注入。bootstrap token 已随首用户注册按设计吊销弃用。
CURLER="$DIND-curl"
docker rm -f "$CURLER" >/dev/null 2>&1 || true
docker run -d --name "$CURLER" --network "$BR_NET" "$CURL_IMAGE" sleep 100000 >/dev/null ||
    fatal "docker run $CURLER"
docker exec "$CURLER" curl -s -o /dev/null "http://10.215.0.10:8420/healthz/liveness" ||
    fatal 'curl helper cannot reach the REST face'
docker exec "$CURLER" curl -s -c /tmp/jar -X POST "http://10.215.0.10:8420/v1/auth/register" \
    -H 'Content-Type: application/json' \
    -d '{"email":"founder@e2e.test","password":"founder-pass-1","display_name":"Founder"}' \
    >/dev/null || fatal 'founder register'
# W2-S5 夹具修正：凭据改铸 **machine 令牌**（平台级凭据 = 资源面 admin
# 等价，rbac-teams §2.3；W2-S4 起平台管理员在资源面被 ResolvePermission
# 短路为只读——founder 的用户 PAT 已不能再承担部署/资源写；S3 模式设置
# 属平台面，机具凭据按 scope 门放行，语义不变）。
S3_TOKEN=$(docker exec "$CURLER" curl -s -b /tmp/jar -X POST "http://10.215.0.10:8420/v1/tokens" \
    -H 'Content-Type: application/json' \
    -d '{"machine":true,"note":"e2e machine token","scopes":["admin"]}' | grep -oE '"token": ?"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "$S3_TOKEN" ] || fatal 'machine token mint failed'
# curl helper 用毕即除（fixture 只承担注册与铸 token；避免钉住 bridge 网络影响后续套件）。
docker rm -f "$CURLER" >/dev/null 2>&1 || true
FOUNDER_TEAM=founder
FOUNDER_PRJ=default
FOUNDER_PROJECT="$FOUNDER_TEAM/$FOUNDER_PRJ"
nl 'founder registered (platform admin); machine token minted; project context '"'"'"$FOUNDER_PROJECT"'"'"''


# ───────────────── A0: 前置校验（label 而 unset → E_S3_NOT_CONFIGURED）
nl '=== A0: honest refusal when s3.mode=unset (E_S3_NOT_CONFIGURED) ==='
cat >"$TMP/app-compose.yaml" <<EOF
name: $APP
services:
  $SVC:
    image: $ALPINE_IMG
    command: ["sleep", "31536000"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 2s
      timeout: 1s
      retries: 100
      start_period: 0s
    labels:
      fleetly.s3: "true"
EOF
stage "$DIND" "$TMP/app-compose.yaml" /opt/fleetly/app-compose.yaml

A0_OUT=$(fcli deploy --timeout 120s /opt/fleetly/app-compose.yaml 2>&1)
case "$A0_OUT" in
*E_S3_NOT_CONFIGURED*) assert "S3-A0 UNSET_DEPLOY_REFUSED" 0 ;;
*) fail "S3-A0 UNSET_DEPLOY_REFUSED" "deploy output missing E_S3_NOT_CONFIGURED: $(printf '%s' "$A0_OUT" | tail -3)" ;;
esac

# ───────────────── A1: RustFS 收敛管理器收敛（E3-5）
nl '=== A1: rustfs manager convergence (managed service + probe) ==='
fcli s3 set --mode rustfs >/dev/null 2>&1 || fatal 's3 set --mode rustfs'
rustfs_running() {
    msh "docker service ps fleetly-rustfs --format '{{.CurrentState}}' | grep -q '^Running'" 2>/dev/null
}
if poll_until 240 rustfs_running; then
    assert "S3-A1 RUSTFS_SERVICE_RUNNING" 0
else
    m docker service ps fleetly-rustfs --no-trunc || true
    assert "S3-A1 RUSTFS_SERVICE_RUNNING" 1 "fleetly-rustfs never ran within 240s"
fi

if poll_until 60 events_grep s3.rustfs_deployed; then
    assert "S3-A2 RUSTFS_DEPLOYED_EVENT" 0
else
    assert "S3-A2 RUSTFS_DEPLOYED_EVENT" 1 "s3.rustfs_deployed missing from event stream"
fi

# 探针（put→get→delete 真实往返）＝托管凭据 + EnsureBucket + 任务 IP 拨号
# 的全链证据；fleetlyd（宿主命名空间）对 pinned-manager 上 running 任务的
# overlay IP 可达。
s3_test_ok() {
    fcli s3 test 2>/dev/null | grep -q 'connection test ok'
}
if poll_until 120 s3_test_ok; then
    assert "S3-A3 PROBE_PUT_GET_DELETE_OK" 0
else
    fcli s3 test || true
    assert "S3-A3 PROBE_PUT_GET_DELETE_OK" 1 "stored-config probe never passed within 120s"
fi

# ───────────────── A2: 注入与牵线（E3-4）
nl '=== A2: credential injection (env + real S3 roundtrip via injected values) ==='
docker exec -d -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$S3_TOKEN" -e FLEETLY_PROJECT="$FOUNDER_PROJECT" \
    "$DIND" sh -c '/opt/fleetly/bin/fleetly deploy --timeout 300s /opt/fleetly/app-compose.yaml > /tmp/s3-deploy.log 2>&1'
deploy_succeeded() {
    fcli deployments list --json "$APP" 2>/dev/null | grep -q '"status": "succeeded"'
}
if poll_until 300 deploy_succeeded; then
    assert "S3-A4 APP_DEPLOY_SUCCEEDED" 0
else
    fcli deployments list --json "$APP" 2>/dev/null || true
    msh 'cat /tmp/s3-deploy.log' || true
    assert "S3-A4 APP_DEPLOY_SUCCEEDED" 1 "deployment never succeeded (status unknown)"
fi

app_ctr() {
    msh "docker ps -q --filter label=com.docker.swarm.service.name=fleetly-$FOUNDER_TEAM-$FOUNDER_PRJ-$APP-$SVC | head -n 1" | tr -d '\r'
}
CTR=$(app_ctr)
[ -n "$CTR" ] || fatal 'app container not found'
EP=$(msh "docker exec $CTR printenv S3_ENDPOINT" | tr -d '\r')
BU=$(msh "docker exec $CTR printenv S3_BUCKET" | tr -d '\r')
PS=$(msh "docker exec $CTR printenv S3_PATH_STYLE" | tr -d '\r')
AK=$(msh "docker exec $CTR printenv S3_ACCESS_KEY_ID" | tr -d '\r')
SK=$(msh "docker exec $CTR printenv S3_SECRET_ACCESS_KEY" | tr -d '\r')
RG=$(msh "docker exec $CTR printenv S3_REGION" | tr -d '\r')
[ "$EP" = "http://rustfs:9000" ] && [ "$BU" = "fleetly" ] && [ "$PS" = "true" ] && [ -n "$AK" ] && [ -n "$SK" ] && [ -z "$RG" ]
assert "S3-A5 INJECTED_ENV_VALUES" $? "endpoint=$EP bucket=$BU pathstyle=$PS ak_set=$([ -n "$AK" ] && echo yes || echo no) sk_set=$([ -n "$SK" ] && echo yes || echo no) region=$RG"

# 真实读写闭环（经注入端点+凭据；restic 一次性容器挂 fleetly-rustfs-net
# ——与上传轨同网络形态；init 幂等容忍重跑，dump 回读逐字节比对）。
MARKER="s3e2e-$(date +%s)"
msh "docker volume rm $APP_VOL" >/dev/null 2>&1 || true
msh "docker volume create $APP_VOL" >/dev/null || fatal 'volume create'
msh "docker run --rm -v $APP_VOL:/data $ALPINE_IMG sh -c \"printf %s $MARKER > /data/marker.txt\"" >/dev/null || fatal 'marker write'
restic_run() { # <args...>
    msh "docker run --rm --network fleetly-rustfs-net -v $APP_VOL:/data:ro -e RESTIC_PASSWORD=s3e2e-pw -e AWS_ACCESS_KEY_ID=$AK -e AWS_SECRET_ACCESS_KEY=$SK $RESTIC_IMG -o s3.bucket-lookup=path -r s3:$EP/$BU/e2e-probe $*"
}
restic_run init --repository-version 2 >/dev/null 2>&1 || true
restic_run backup /data --json >/dev/null 2>&1 || fatal 'restic backup failed'
GOT=$(restic_run dump latest /data/marker.txt 2>/dev/null | tr -d '\r\n')
[ "$GOT" = "$MARKER" ]
assert "S3-A6 INJECTED_ENDPOINT_READ_WRITE" $? "want=$MARKER got=$GOT (restic roundtrip over the injected endpoint+credentials)"

# ───────────────── A3: 状态备份上传轨（E3-3 × E3-5 接线）
nl '=== A3: state backup upload over the managed endpoint (upload_status=ok) ==='
# 手动触发（客户端 30s deadline 在慢后端下可能先行超时——服务端已脱钩
# 客户端取消，备份本体照常执行并落账）；上传结论以台账轮询为准。
BEFORE_ID=$(fcli backups list --json 2>/dev/null | grep -o '"id": *"[^"]*"' | head -n 1 | sed 's/.*: *"//; s/"$//')
fcli backups create >"$TMP/backup-create.log" 2>&1 || true
manual_ok() {
    fcli backups list --json 2>/dev/null | grep -q '"upload_status": *"ok"'
}
if poll_until 300 manual_ok; then
    assert "S3-A7 BACKUP_UPLOAD_OK" 0
else
    fcli backups list --json || true
    cat "$TMP/backup-create.log" || true
    assert "S3-A7 BACKUP_UPLOAD_OK" 1 "no upload_status=ok row within 300s"
fi
# 手动触发的新行落账（id 推进 + 结论 ok）——同步响应丢失不掩盖事实。
manual_row_ok() {
    fcli backups list --json 2>/dev/null | grep -o '"id": *"[^"]*"' | head -n 1 | grep -qv "$BEFORE_ID" &&
        fcli backups list --json 2>/dev/null | grep -q '"upload_status": *"ok"'
}
if poll_until 120 manual_row_ok; then
    assert "S3-A8 MANUAL_BACKUP_UPLOAD_OK" 0
else
    fcli backups list --json || true
    assert "S3-A8 MANUAL_BACKUP_UPLOAD_OK" 1 "manual trigger never landed an ok row"
fi

# ───────────────── A3.5: 状态面诚实口径（D-S3-8，rustfs 启用态）
nl '=== A3.5: s3 status honesty note (rustfs mode) ==='
RUSTFS_STATUS_OUT=$(fcli s3 status 2>&1 || true)
case "$RUSTFS_STATUS_OUT" in
*"convenience layer"*"NOT disaster recovery"*) assert "S3-A11 STATUS_HONESTY_NOTE" 0 ;;
*) fail "S3-A11 STATUS_HONESTY_NOTE" "status output: $(printf '%s' "$RUSTFS_STATUS_OUT" | tail -5)" ;;
esac

# ───────────────── A4: 禁用语义（服务移除、卷保留）
nl '=== A4: disable (service removed, volume retained) ==='
fcli s3 set --mode unset >/dev/null 2>&1 || fatal 's3 set --mode unset'
rustfs_gone() {
    msh 'docker service ls --format "{{.Name}}"' 2>/dev/null | grep -q '^fleetly-rustfs$'
    [ $? -ne 0 ]
}
if poll_until 120 rustfs_gone; then
    assert "S3-A9 DISABLE_REMOVES_SERVICE" 0
else
    m docker service ls || true
    assert "S3-A9 DISABLE_REMOVES_SERVICE" 1 "fleetly-rustfs still present 120s after unset"
fi
msh "docker volume ls --format '{{.Name}}'" 2>/dev/null | grep -q "^fleetly-rustfs-data$"
assert "S3-A10 VOLUME_RETAINED" $? "data volume must survive disable (delete explicitly to discard)"

# ───────────────── A5: 状态面（禁用后的部署态行）
nl '=== A5: s3 status deployment line after disable ==='
STATUS_OUT=$(fcli s3 status 2>&1 || true)
case "$STATUS_OUT" in
*"deployment: not deployed"*) assert "S3-A12 STATUS_DEPLOYMENT_LINE" 0 ;;
*) fail "S3-A12 STATUS_DEPLOYMENT_LINE" "status missing deployment line after disable: $(printf '%s' "$STATUS_OUT" | tail -5)" ;;
esac

finish
