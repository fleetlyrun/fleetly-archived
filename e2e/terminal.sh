#!/bin/sh
# e2e/terminal.sh — Web 终端端到端（E7 W5-S6；设计
# docs/design/2026-09-22-web-terminal.md §2 + §4 安全面套件；control-plane-tls.sh
# 同骨架——单 dind、编排自足、镜像钉 digest）：
#
#   镜像（钉定口径，2026-09-22 CI 首推后收紧）：fleetly-exec 是 ghcr 包，
#   dind 内直拉 tag@digest 全引用（与 Go 常量逐字一致——真实 pull 才落
#   RepoDigests，save|load/本地构建解析不了 digest 引用；databases.sh 的
#   DB_GHCR_* 同款纪律）。凭据 TERM_GHCR_USER/TERM_GHCR_TOKEN 可选：有 →
#   登录直拉（CI 的 GITHUB_TOKEN 自动具备 packages:read）；无 → 匿名直拉
#   （2026-09-24 实证匿名可读，本地复跑路径），拉取失败仍诚实 fatal。
#
#   T-1  relay 收敛管理器收敛：fleetly-exec global 服务 running（每节点一任务）。
#   T-2  terminal status RPC：enabled + relay 已连接（nodes_connected ≥ 1
#        ——反向常连 + 成员发现的端到端证据）。
#   T-3  受管应用部署成功（fleetly.app label 容器就位——会话目标的资格面）。
#   T-4  **真 PTY 回显**：termclient 发送 `echo term-ok-<rand>`，alpine 容器
#        （无 bash → shell 白名单探测退 /bin/sh）回显命中（真实 exec 路径）。
#   T-5  terminal 独立 scope：read-only token 取票拒（HTTP 403）。
#   T-6  terminal scope 独立 token：tokens create --scopes terminal → 全会
#        话流走通（read/deploy 不蕴含 terminal 的正向面）。
#   T-7  ticket 一次性：同 ticket 二连拒（升级 401——重放拒绝）。
#   T-8  per-token 并发上限：双 hold 会话占满 → 第 3 条 close 429。
#   T-9  起止事件：terminal.opened / terminal.closed 命中事件流（与审计行
#        同事务 Outbox——事件面即审计披露面；审计行本体由单测直读断言）。
#   T-10 会话内容零泄漏：PTY 回显 marker 绝不出现在事件 payload（负向）。
#
#   诚实范围（单测/e2e 分工）：
#   - label 卫兵 403（无 fleetly.app 容器拒绝）在 relay 单测钉死（fake
#     docker）——控制面不暴露任意容器选择（会话目标 = 平台选的 task），
#     e2e 以「会话只能落在受管 app task」结构性覆盖；
#   - 空闲 10min / 硬上限 30min 由单测注入缝钉死（e2e 不等真实时限）；
#   - 集群 token 错误拒在单测钉死（e2e 不伪造 secret 重启 relay）；
#   - terminal.enabled=false 的 E_TERMINAL_DISABLED 面：同一 swarm 上不可
#     起第二个禁用终端的 fleetlyd（其收敛管理器会移除 T-1 依赖的服务）——单测
#     覆盖，e2e 不做。
#
# 断言风格与 e2e/control-plane-tls.sh 一致（T-x: PASS/FAIL 行 + TERM_FAIL
# 计数 + finish）。
# usage: e2e/terminal.sh
# env:
#   T_DIND_IMAGE   dind 镜像（默认 docker:29.8.1-dind，钉 digest 与 CI 一致）
#   T_SKIP_BUILD   1 = 跳过交叉编译，改用 T_BIN_DIR 下的现成二进制
#   T_BIN_DIR      T_SKIP_BUILD=1 时的二进制来源（fleetlyd/fleetly/termclient）
#   T_VERSION      注入的版本串（默认 v0.2.0-terminal-e2e）
#   TERM_GHCR_USER / TERM_GHCR_TOKEN  exec 镜像的 ghcr 凭据（可选——packages:read；CI 自动注入；缺省匿名直拉）
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

# ── 镜像钉 digest（T0-V2.3 供应链；与 e2e/control-plane-tls.sh 同源）。
DIND_IMAGE="${T_DIND_IMAGE:-docker:29.8.1-dind@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0}"
ALPINE_IMG='alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc'
# curl helper（v0.3 fixture：REST 注册/铸 PAT 面——auth.sh 同源钉版）。
CURL_IMAGE='curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69'
T_SKIP_BUILD="${T_SKIP_BUILD:-0}"
T_BIN_DIR="${T_BIN_DIR:-}"
T_VERSION="${T_VERSION:-v0.2.0-terminal-e2e}"

BR_NET=fleetly-t-br
BR_SUBNET=10.220.0.0/24
DIND=fleetly-t-e2e-dind
# Go 常量 DefaultExecRelayImage 的逐字形态（tag@digest 全引用——真实 pull
# 落 RepoDigests 后收敛管理器的钉定引用才可解析）。
EXEC_IMAGE='ghcr.io/fleetlyrun/fleetly-exec:v0.2.0-exec.1@sha256:4de40017c620b55b76048c3369f64b3a747c875a4f7bf21ac8c6f88bc4b0793b'

APP=termapp
SVC=web
MARKER="term-ok-$(date +%s)-$RANDOM"

TERM_FAIL=0
SUITE_DINDS=''
ACTIVE_NET=''

tl() { printf '[term-e2e %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
pass() { tl "$1: PASS"; }
fail() {
    tl "$1: FAIL ${2:-}"
    # shellcheck disable=SC2034
    TERM_FAIL=$((TERM_FAIL + 1))
}
assert() { # <name> <0|1> [detail]
    if [ "$2" -eq 0 ]; then pass "$1"; else fail "$1" "${3:-}"; fi
}
fatal() { tl "FATAL $*"; exit 1; }
finish() {
    if [ "$TERM_FAIL" -eq 0 ]; then
        tl "SUITE-DONE all-asserts-passed"
        exit 0
    fi
    tl "SUITE-DONE failed-asserts=$TERM_FAIL"
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
    docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$CTL_TOKEN" \
        -e FLEETLY_PROJECT="$FOUNDER_PROJECT" \
        "$DIND" /opt/fleetly/bin/fleetly "$@"
}
# tcli <args...> — dind 内的 termclient（WS 测试客户端；HTTP 面）。
tcli() {
    docker exec "$DIND" /opt/fleetly/bin/termclient -addr 127.0.0.1:8420 "$@"
}
# events_grep <pattern> — 事件流快照检索（watch 快照即「迄今全部事件」；
# busybox timeout 掐断 follow 流）。
events_grep() {
    docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$CTL_TOKEN" \
        "$DIND" sh -c 'timeout 4 /opt/fleetly/bin/fleetly events watch --since-seq 0 > /tmp/term-events.txt 2>/dev/null; exit 0'
    m grep -q "$1" /tmp/term-events.txt
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
        tl "suite RED (rc=$rc) — dumping dind log tail"
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
command -v go >/dev/null 2>&1 || T_SKIP_BUILD=1

# ─────────────────────────────────────────────────────────────── 构建
if [ "$T_SKIP_BUILD" != '1' ]; then
    tl "cross-compiling linux/amd64 fleetlyd+fleetly+termclient ($T_VERSION)"
    (
        cd "$ROOT" &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$T_VERSION" \
                -o "$TMP/fleetlyd" ./cmd/fleetlyd &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$T_VERSION" \
                -o "$TMP/fleetly" ./cmd/fleetly &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w" \
                -o "$TMP/termclient" ./e2e/termclient
    ) || fatal 'go build failed'
    T_BIN_DIR="$TMP"
else
    T_BIN_DIR="${T_BIN_DIR:?T_SKIP_BUILD=1 requires T_BIN_DIR}"
    tl "using prebuilt binaries from $T_BIN_DIR"
fi
for b in fleetlyd fleetly termclient; do
    [ -f "$T_BIN_DIR/$b" ] || fatal "$b missing in $T_BIN_DIR"
done

# ─────────────────────────────────────────────────────── dind 编排准备
leftovers=$(docker ps -aq --filter name=fleetly-t-e2e- 2>/dev/null || true)
if [ -n "$leftovers" ]; then
    tl "WARN removing leftover term-e2e containers: $leftovers"
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
tl "dind $DIND ready (engine $(docker exec "$DIND" docker version --format '{{.Server.Version}}' 2>/dev/null))"

tl 'staging binaries + compose (exec+stdin)'
docker exec "$DIND" mkdir -p /opt/fleetly/bin /opt/fleetly/etc /var/lib/fleetly || fatal 'mkdir stage'
stage "$DIND" "$T_BIN_DIR/fleetlyd" /opt/fleetly/bin/fleetlyd
stage "$DIND" "$T_BIN_DIR/fleetly" /opt/fleetly/bin/fleetly
stage "$DIND" "$T_BIN_DIR/termclient" /opt/fleetly/bin/termclient
docker exec "$DIND" chmod +x /opt/fleetly/bin/fleetlyd /opt/fleetly/bin/fleetly /opt/fleetly/bin/termclient || fatal 'chmod'

# fleetly-exec 镜像：ghcr 包 digest 全引用 dind 内直拉（databases.sh 的
# DB_GHCR_* 同款纪律——真实 pull 才落 RepoDigests，本地构建解析不了钉定
# 引用）。凭据经 exec env 注入，不落 argv 之外的面；无凭据 → 匿名直拉
# （2026-09-24 实证该包匿名可读——本地复跑路径），拉取失败仍诚实 fatal。
if [ -n "${TERM_GHCR_USER:-}" ] && [ -n "${TERM_GHCR_TOKEN:-}" ]; then
    docker exec -e GHCR_USER="$TERM_GHCR_USER" -e GHCR_TOKEN="$TERM_GHCR_TOKEN" \
        "$DIND" sh -c 'printf %s "$GHCR_TOKEN" | docker login ghcr.io -u "$GHCR_USER" --password-stdin >/dev/null' ||
        fatal 'docker login ghcr.io (inside dind) failed'
    docker exec "$DIND" docker pull -q "$EXEC_IMAGE" >/dev/null ||
        fatal "pull $EXEC_IMAGE (inside dind) failed — check the ghcr credential and its packages:read access to fleetlyrun/fleetly-exec"
    docker exec "$DIND" docker logout ghcr.io >/dev/null 2>&1 || true
    tl "exec image pulled inside dind with ghcr credentials ($EXEC_IMAGE)"
else
    docker exec "$DIND" docker pull -q "$EXEC_IMAGE" >/dev/null ||
        fatal "pull $EXEC_IMAGE (inside dind, anonymous) failed — set TERM_GHCR_USER/TERM_GHCR_TOKEN (a ghcr credential with packages:read on fleetlyrun/fleetly-exec) if the package is private for your principal. In CI the terminal-e2e job passes GITHUB_TOKEN automatically."
    tl "exec image pulled inside dind anonymously ($EXEC_IMAGE)"
fi

# fleetlyd 配置（TLS off = ws:// 明文形态——exec 通道明文降级面；离线 dind
# ACME/git 关闭）。
cat >"$TMP/config.yaml" <<'EOF'
addr: "0.0.0.0:8420"
grpc:
  addr: "0.0.0.0:8421"
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
logging:
  level: info
EOF
stage "$DIND" "$TMP/config.yaml" /opt/fleetly/etc/config.yaml

docker exec "$DIND" docker swarm init --advertise-addr eth0 >/dev/null || fatal 'swarm init'

# 应用镜像预拉（logs-victorialogs.sh 同款——部署前的确定性 pull；引擎对
# 缺镜像的受理是快速失败面，预拉使部署时序与本套件的断言解耦）。
tl "pre-pulling app image ($ALPINE_IMG)"
msh "docker pull $ALPINE_IMG" >/dev/null 2>&1 || fatal 'app image pre-pull failed'

docker exec "$DIND" sh -c 'cd /var/lib/fleetly && nohup /opt/fleetly/bin/fleetlyd -c /opt/fleetly/etc/config.yaml > /tmp/term-fleetlyd.log 2>&1 & echo $! > /var/run/fleetlyd.pid'
CTL_TOKEN=''
CTL_LIVE=0
i=0
while [ "$i" -lt 90 ]; do
    if msh 'wget -q -T 3 -O /dev/null http://127.0.0.1:8420/healthz/liveness' 2>/dev/null; then
        CTL_LIVE=1
        break
    fi
    i=$((i + 2))
    sleep 2
done
[ "$CTL_LIVE" -eq 1 ] || {
    msh 'tail -40 /tmp/term-fleetlyd.log' || true
    fatal 'fleetlyd not live within 90s'
}
CTL_TOKEN=$(m sh -c 'cat /var/lib/fleetly/bootstrap-token') || fatal 'read bootstrap token'
[ -n "$CTL_TOKEN" ] || fatal 'empty bootstrap token'
tl 'fleetlyd live (plaintext faces), bootstrap token read'

# ── v0.3 归属管道 fixture（rbac-teams §2.1/§2.3/§3.4）：注册 founder（首
# 用户 = 平台管理员 + 个人队 + 默认项目 default）→ 会话自服务铸用户 PAT
#（admin scope；CLI 不消费会话 cookie）。首次部署经
# FLEETLY_PROJECT=founder/default 显式携带项目归属（fcli 统一 env 注入）。
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
# W2-S5 夹具修正：主凭据改铸 **machine 令牌**（平台级凭据 = 资源面 admin
# 等价，rbac-teams §2.3；W2-S4 起平台管理员在资源面被 ResolvePermission
# 短路为只读——founder 的用户 PAT 已不能再承担部署）。终端 scope 的独立
# 令牌子断言在 T-7 段自行铸造（见下文）。
CTL_TOKEN=$(docker exec "$CURLER" curl -s -b /tmp/jar -X POST "http://10.220.0.10:8420/v1/tokens" \
    -H 'Content-Type: application/json' \
    -d '{"machine":true,"note":"e2e machine token","scopes":["admin"]}' | grep -oE '"token": ?"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "$CTL_TOKEN" ] || fatal 'machine token mint failed'
# curl helper 用毕即除（fixture 只承担注册与铸 token；避免钉住 bridge 网络影响后续套件）。
docker rm -f "$CURLER" >/dev/null 2>&1 || true
FOUNDER_TEAM=founder
FOUNDER_PRJ=default
FOUNDER_PROJECT="$FOUNDER_TEAM/$FOUNDER_PRJ"
tl 'founder registered (platform admin); machine token minted; project context '"$FOUNDER_PROJECT"

# ───────── T-1: relay 收敛管理器收敛（global 服务 running）
t1_running() {
    msh 'docker service ls --filter name=fleetly-exec --format "{{.Replicas}}" 2>/dev/null' | grep -q '^1/1$'
}
if poll_until 120 t1_running; then
    assert "T-1 EXEC_RELAY_SERVICE_RUNNING" 0
else
    msh 'docker service ls || true'
    msh 'docker service ps fleetly-exec --no-trunc 2>/dev/null | tail -5' || true
    assert "T-1 EXEC_RELAY_SERVICE_RUNNING" 1 "fleetly-exec never converged to 1/1"
fi

# ───────── T-2: terminal status RPC（enabled + relay 已连接——反向常连证据）
t2_status() {
    m wget -q -T 5 -O - --header="Authorization: Bearer $CTL_TOKEN" \
        "http://127.0.0.1:8420/v1/terminal/status" 2>/dev/null
}
t2_ready() {
    # protojson 的冒号后空格非确定（随机空白）——两种形态都容忍。
    t2_status | grep -qE '"enabled": ?true' && t2_status | grep -qE '"nodes_connected": ?[1-9]'
}
if poll_until 60 t2_ready; then
    assert "T-2 TERMINAL_STATUS_RPC_RELAY_CONNECTED" 0
else
    t2_status || true
    assert "T-2 TERMINAL_STATUS_RPC_RELAY_CONNECTED" 1 "status never showed an enabled terminal with a connected relay"
fi

# ───────── T-3: 受管应用部署（fleetly.app label 容器就位）
cat >"$TMP/app-compose.yaml" <<EOF
name: $APP
services:
  $SVC:
    image: $ALPINE_IMG
    command: ["sh", "-c", "sleep 100000"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 2s
      timeout: 1s
      retries: 100
      start_period: 0s
EOF
stage "$DIND" "$TMP/app-compose.yaml" /opt/fleetly/termapp-compose.yaml
docker exec -d -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$CTL_TOKEN" -e FLEETLY_PROJECT="$FOUNDER_PROJECT" \
    "$DIND" sh -c '/opt/fleetly/bin/fleetly deploy --timeout 300s /opt/fleetly/termapp-compose.yaml > /tmp/term-deploy.log 2>&1'
deploy_succeeded() {
    fcli deployments list --json "$APP" 2>/dev/null | grep -q '"status": "succeeded"'
}
if poll_until 300 deploy_succeeded; then
    assert "T-3 MANAGED_APP_DEPLOY_SUCCEEDED" 0
else
    fcli deployments list --json "$APP" 2>/dev/null || true
    msh 'cat /tmp/term-deploy.log' || true
    assert "T-3 MANAGED_APP_DEPLOY_SUCCEEDED" 1 "deployment never succeeded"
fi

# ───────── T-4: 真 PTY 回显（alpine 无 bash → 白名单探测退 /bin/sh；
#         发送 echo 命令，容器回显命中 marker——真实 exec 路径）
t4_echo() {
    tcli -mode run -token "$CTL_TOKEN" -app "$APP" -service "$SVC" \
        -command "echo $MARKER" -expect "$MARKER" >/tmp/term-t4.out 2>/tmp/term-t4.err
}
if poll_until 60 t4_echo; then
    assert "T-4 REAL_PTY_ECHO_ROUNDTRIP" 0
else
    tl "termclient stdout: $(m cat /tmp/term-t4.out 2>/dev/null | tail -1)"
    tl "termclient stderr: $(m cat /tmp/term-t4.err 2>/dev/null | tail -2 | tr '\n' ' ')"
    assert "T-4 REAL_PTY_ECHO_ROUNDTRIP" 1 "echo marker never echoed back within 60s"
fi

# ───────── T-5: terminal scope 缺失拒（read-only token → HTTP 403）
# protojson 的冒号后空格非确定（随机空白）——grep -E 兼容两种形态。
# W2-S5：独立 scope 令牌经 --machine 铸造（主凭据 = admin 机具令牌，可铸
# 任意 scope 的平台级令牌——S4 起平台管理员用户 PAT 的可达集不再含 terminal）。
READ_TOKEN=$(fcli tokens create --machine --scopes read --note e2e-term-read --json 2>/dev/null | grep -oE '"token": ?"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "$READ_TOKEN" ] || fatal 'read token creation failed'
t5_rejected() {
    if tcli -mode ticket -token "$READ_TOKEN" -app "$APP" -service "$SVC" >/dev/null 2>&1; then
        return 1
    fi
    return 0
}
if poll_until 30 t5_rejected; then
    assert "T-5 READ_TOKEN_TICKET_REJECTED" 0
else
    assert "T-5 READ_TOKEN_TICKET_REJECTED" 1 "read-only token unexpectedly obtained a terminal ticket"
fi

# ───────── T-6: terminal scope 独立 token 全会话走通（--machine 铸造，同 T-5）
TERM_TOKEN=$(fcli tokens create --machine --scopes terminal --note e2e-term-scope --json 2>/dev/null | grep -oE '"token": ?"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "$TERM_TOKEN" ] || fatal 'terminal-scope token creation failed'
t6_echo() {
    tcli -mode run -token "$TERM_TOKEN" -app "$APP" -service "$SVC" \
        -command "echo scoped-$MARKER" -expect "scoped-$MARKER" >/dev/null 2>&1
}
if poll_until 60 t6_echo; then
    assert "T-6 TERMINAL_SCOPE_TOKEN_SESSION" 0
else
    assert "T-6 TERMINAL_SCOPE_TOKEN_SESSION" 1 "explicit terminal-scope token could not run a session"
fi

# ───────── T-7: ticket 一次性（同 ticket 二连拒——重放 401）
TICKET=$(tcli -mode ticket -token "$CTL_TOKEN" -app "$APP" -service "$SVC" 2>/dev/null | tr -d '[:space:]')
[ -n "$TICKET" ] || fatal 'ticket acquisition failed'
# 首次消费（正常会话：回显成立）。
t7_first() {
    tcli -mode run -ticket "$TICKET" -command "echo first-$MARKER" -expect "first-$MARKER" >/dev/null 2>&1
}
poll_until 30 t7_first || fatal 'first ticket consumption failed (pre-condition)'
# 二连（重放）→ 升级拒绝。
t7_replay() {
    if tcli -mode run -ticket "$TICKET" -command "echo replay" -expect "replay" >/dev/null 2>&1; then
        return 1
    fi
    return 0
}
if poll_until 30 t7_replay; then
    assert "T-7 TICKET_REPLAY_REJECTED" 0
else
    assert "T-7 TICKET_REPLAY_REJECTED" 1 "a spent ticket was accepted again"
fi

# ───────── T-8: per-token 并发上限（双 hold 占满 → 第 3 条 close 429）
docker exec -d -e FLEETLY_ADDR=127.0.0.1:8421 "$DIND" sh -c "/opt/fleetly/bin/termclient -mode hold -token $CTL_TOKEN -app $APP -service $SVC -hold-seconds 60 >/tmp/hold1.err 2>&1"
docker exec -d -e FLEETLY_ADDR=127.0.0.1:8421 "$DIND" sh -c "/opt/fleetly/bin/termclient -mode hold -token $CTL_TOKEN -app $APP -service $SVC -hold-seconds 60 >/tmp/hold2.err 2>&1"
t8_saturated() {
    t2_status | grep -qE '"active_sessions": ?[2-9]'
}
if ! poll_until 45 t8_saturated; then
    tl "t2_status: $(t2_status)"
    tl "hold1.err: $(m cat /tmp/hold1.err 2>/dev/null | tail -2 | tr '\n' ' ')"
    tl "hold2.err: $(m cat /tmp/hold2.err 2>/dev/null | tail -2 | tr '\n' ' ')"
    assert "T-8a PER_TOKEN_LIMIT_SATURATED" 1 "two hold sessions never saturated the per-token quota"
else
    assert "T-8a PER_TOKEN_LIMIT_SATURATED" 0
fi
t8_third_rejected() {
    tcli -mode run -token "$CTL_TOKEN" -app "$APP" -service "$SVC" \
        -command "echo third" -expect-close 429 >>/tmp/hold3.err 2>&1
}
if poll_until 30 t8_third_rejected; then
    assert "T-8b THIRD_SESSION_CLOSE_429" 0
else
    tl "hold3.err: $(m tail -2 /tmp/hold3.err 2>/dev/null | tr '\n' ' ')"
    assert "T-8b THIRD_SESSION_CLOSE_429" 1 "the third session was not rejected with close code 429"
fi
# 等 hold 会话自然收尾（60s hold），不拖累后续断言。
msh 'sleep 45; true' >/dev/null 2>&1 || true

# ───────── T-9: 起止事件（terminal.opened / terminal.closed——与审计行同
#         事务 Outbox；事件面即审计披露面，审计行本体由单测直读）
if poll_until 60 events_grep terminal.opened; then
    assert "T-9a TERMINAL_OPENED_EVENT" 0
else
    assert "T-9a TERMINAL_OPENED_EVENT" 1 "terminal.opened missing from the event stream"
fi
if poll_until 60 events_grep terminal.closed; then
    assert "T-9b TERMINAL_CLOSED_EVENT" 0
else
    assert "T-9b TERMINAL_CLOSED_EVENT" 1 "terminal.closed missing from the event stream"
fi

# ───────── T-10: 会话内容零泄漏（PTY 回显 marker 绝不进事件 payload——
#          明文纪律负向；重取快照含全部事件后检索 marker）
if events_grep "$MARKER"; then
    assert "T-10 SESSION_CONTENT_NOT_IN_EVENTS" 1 "PTY echo content leaked into the event stream"
else
    assert "T-10 SESSION_CONTENT_NOT_IN_EVENTS" 0
fi

finish
