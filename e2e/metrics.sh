#!/bin/sh
# e2e/metrics.sh — E6 W5-S3 托管 metrics 三件套端到端（单节点 dind 形态；
# 设计 docs/design/2026-09-22-observability.md §4 的真机闭环，D-W5-2
# opt-in；logs-victorialogs.sh 同骨架——编排自足、镜像钉 digest）：
#
#   交叉编译 linux/amd64 fleetlyd+fleetly（或复用 MET_BIN_DIR）→ 宿主 bridge
#   上起一个特权 dind（私网 10.217.0.0/24，镜像钉 digest）→ swarm init +
#   fleetlyd 起服 → 全部断言经 dind 内的 docker/fleetly CLI/REST 驱动：
#
#   A1 缺省零常驻（D-W5-2 opt-in：metrics.mode 未显式设置 = unset 生效）：
#      收敛拍后三件服务全不在 + `metrics status` 读 unset。
#   A2 `metrics mode set on` 受理（deploy scope 走 bootstrap token 通畅）。
#   A3 三件收敛（fleetly-cadvisor / fleetly-node-exporter / fleetly-
#      victoriametrics 全部 Running；首跑含镜像拉取，重试窗放宽）。
#   A4 VM 回环健康（host 网络任务 + -httpListenAddr=127.0.0.1:8428——
#      D-W5-4 等价承载形态，见 internal/metrics/spec.go 头注记；查询面
#      零公网面不变量本票修订后仍钉死）。
#   A5 采集器回环在位（cAdvisor 8080 / node_exporter 9100 的 /metrics 经
#      127.0.0.1 可达——0.0.0.0 绑定含回环，同命名空间冒烟）。
#   A6 VM 抓取收敛：`metrics status` nodes_reporting 1/1（up 计数的诚实
#      口径——单节点 dind = 1 节点全报）。
#   A7 指标查询面（PromQL 透传——操作员工具）：up 序列含 cadvisor 与
#      node_exporter 两个 job（实例为节点 advertise 地址目标）。
#   A8 节点维度序列（node_load1 非空）。
#   A9 容器维度序列（container_memory_usage_bytes 非空）。
#   A10 应用归属标签实测形态：swarm service 维度 = container_label_com_
#      docker_swarm_service_name（compose 维度标签不适用——设计 §4.2 注记
#      的真机核实面），whoami 应用部署后按 fleetly-<app>-<svc> 命中。
#   A11 CLI `metrics query` 人读形态命中（--since/--step 旗标在位置参数前）。
#   A12 `metrics status` on 态全景（三件 deployed + on + 1/1）。
#   A13 `metrics mode set unset` → 三件移除 + 数据卷保留 + stack_removed
#      事件语义（status 读 unset）。
#   A14 关闭后查询诚实报错：`metrics query` → E_METRICS_NOT_ENABLED
#      （不返回空序列冒充有数）。
#
# §6 挂账票「跨节点 metrics 采集修订」新增断言（2026-09-23）：
#   A20 采集器绑定面 = 0.0.0.0（cAdvisor -listen_ip / node_exporter
#      --web.listen-address 服务 spec 字面钉死）+ VM -httpListenAddr 恒
#      回环 8428（绑定面分野双向钉死）。
#   A21 经节点 advertise 地址直连采集器 /metrics 可达（manager → 节点
#      地址 = 跨节点抓取路径的单机缩影——VPC/LAN 直连，不依赖 overlay）。
#   A22 VM 的抓取 config 对象内容含 advertise 地址 targets（动态 targets
#      ——<addr>:8080 + <addr>:9100；单节点 dind = 1 节点）。
#   A23 `metrics status` on 态出现新诚实 note（采集面 = 内网面，公网访问
#      由节点防火墙负责——spec 注释 / status note / Console 文案三处同锚）。
#
# 断言风格与 e2e/logs-victorialogs.sh 一致（MET-x: PASS/FAIL 行 + NL_FAIL
# 计数 + finish）。
# usage: e2e/metrics.sh
# env:
#   MET_DIND_IMAGE   dind 镜像（默认 docker:29.8.1-dind，钉 digest 与 CI 一致）
#   MET_SKIP_BUILD   1 = 跳过交叉编译，改用 MET_BIN_DIR 下的现成二进制
#   MET_BIN_DIR      MET_SKIP_BUILD=1 时的二进制来源（需含 fleetlyd 与 fleetly）
#   MET_VERSION      注入的版本串（默认 v0.2.0-metrics-e2e）
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

# ── 镜像钉 digest（T0-V2.3 供应链；台账 docs/runbooks/image-prepull.md
#    #15/#16/#17——与 Go 常量 internal/metrics/spec.go 三常量同源，改动须
#    多处同步）。cAdvisor 官方 repo = gcr.io/cadvisor/cadvisor（Docker Hub
#    google/cadvisor 已 DEPRECATED——repo 勘误见台账 #17）。
DIND_IMAGE="${MET_DIND_IMAGE:-docker:29.8.1-dind@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0}"
CURL_IMAGE='curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69'
VM_IMG='victoriametrics/victoria-metrics:v1.152.0@sha256:86ca5fdb6d87d56ba047b044039019ba2bd9042b36e35f6ea34e437b6c825cef'
NODE_EXPORTER_IMG='prom/node-exporter:v1.12.1@sha256:1b4e4438faca4dd7e001dd445d161a4a2091b0fededa84093b3a8dfeae1f1be0'
CADVISOR_IMG='gcr.io/cadvisor/cadvisor:v0.55.1@sha256:3de2bd5203120b866d74a9b283b2ffb8ec382fbf9dc321814700c6ea6f44ec57'
# W5-S2（D-V3W5-1）：vmalert 组件（alerts.mode=on 的规则评估器；台账 #21）。
# S4 告警 e2e 腿的预拉面——本腿只拉取暖机，不驱动告警链。
VMALERT_IMG='victoriametrics/vmalert:v1.152.0@sha256:ba00566373eb8c72d70cbee123e27ee75292dc0239830f36218c72712ba396b2'
WHOAMI_IMG='traefik/whoami:v1.10.4@sha256:02d8fe035f170f91cbb5e458a57f4cefab747436f8244a0eb2d66785fe5e565f'
MET_SKIP_BUILD="${MET_SKIP_BUILD:-0}"
MET_BIN_DIR="${MET_BIN_DIR:-}"
MET_VERSION="${MET_VERSION:-v0.2.0-metrics-e2e}"

BR_NET=fleetly-m-br
BR_SUBNET=10.217.0.0/24
DIND=fleetly-m-e2e-dind
APP=metricsapp
SVC=web

NL_FAIL=0
SUITE_DINDS=''
ACTIVE_NET=''

nl() { printf '[met-e2e %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
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
    docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_PROJECT="$FOUNDER_PROJECT" -e FLEETLY_TOKEN="$MET_TOKEN" \
        "$DIND" /opt/fleetly/bin/fleetly "$@"
}
# events_grep <pattern> — 现抓事件流快照（busybox timeout 掐断 follow 流）
# 并在快照中检索。watch 是长驻流，快照即「迄今全部事件」。
events_grep() {
    docker exec -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$MET_TOKEN" \
        "$DIND" sh -c 'timeout 4 /opt/fleetly/bin/fleetly events watch --since-seq 0 > /tmp/met-events.txt 2>/dev/null; exit 0'
    m grep -q "$1" /tmp/met-events.txt
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
command -v go >/dev/null 2>&1 || MET_SKIP_BUILD=1

# ─────────────────────────────────────────────────────────────── 构建
if [ "$MET_SKIP_BUILD" != '1' ]; then
    nl "cross-compiling linux/amd64 fleetlyd+fleetly ($MET_VERSION)"
    (
        cd "$ROOT" &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$MET_VERSION" \
                -o "$TMP/fleetlyd" ./cmd/fleetlyd &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$MET_VERSION" \
                -o "$TMP/fleetly" ./cmd/fleetly
    ) || fatal 'go build failed'
    MET_BIN_DIR="$TMP"
else
    MET_BIN_DIR="${MET_BIN_DIR:?MET_SKIP_BUILD=1 requires MET_BIN_DIR}"
    nl "using prebuilt binaries from $MET_BIN_DIR"
fi
[ -f "$MET_BIN_DIR/fleetlyd" ] || fatal "fleetlyd missing in $MET_BIN_DIR"
[ -f "$MET_BIN_DIR/fleetly" ] || fatal "fleetly missing in $MET_BIN_DIR"

# ─────────────────────────────────────────────────────── dind 编排准备
leftovers=$(docker ps -aq --filter name=fleetly-m- 2>/dev/null || true)
if [ -n "$leftovers" ]; then
    nl "WARN removing leftover met-e2e containers: $leftovers"
    echo "$leftovers" | xargs docker rm -f >/dev/null 2>&1 || true
fi
docker network rm "$BR_NET" >/dev/null 2>&1 || true
docker network create -d bridge --subnet "$BR_SUBNET" "$BR_NET" >/dev/null || fatal "create $BR_NET"
ACTIVE_NET="$BR_NET"

docker rm -f "$DIND" >/dev/null 2>&1 || true
docker run -d --name "$DIND" --privileged --hostname mgr \
    --network "$BR_NET" --ip 10.217.0.10 "$DIND_IMAGE" >/dev/null ||
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
stage "$DIND" "$MET_BIN_DIR/fleetlyd" /opt/fleetly/bin/fleetlyd
stage "$DIND" "$MET_BIN_DIR/fleetly" /opt/fleetly/bin/fleetly
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

# 预拉三件套+vmalert 钉版镜像（一次拉齐，收敛窗只做调度；whoami 随后用）。
nl 'pre-pulling fixture images (pinned digests)'
for img in "$VM_IMG" "$NODE_EXPORTER_IMG" "$CADVISOR_IMG" "$VMALERT_IMG" "$WHOAMI_IMG"; do
    docker exec "$DIND" docker pull -q "$img" >/dev/null || fatal "pull $img"
done

docker exec "$DIND" docker swarm init --advertise-addr eth0 >/dev/null || fatal 'swarm init'

docker exec "$DIND" sh -c 'cd /var/lib/fleetly && nohup /opt/fleetly/bin/fleetlyd -c /opt/fleetly/etc/config.yaml > /tmp/met-fleetlyd.log 2>&1 & echo $! > /var/run/fleetlyd.pid'
MET_LIVE=0
i=0
while [ "$i" -lt 90 ]; do
    if m sh -c 'wget -q -T 3 -O /dev/null http://127.0.0.1:8420/healthz/liveness' 2>/dev/null; then
        MET_LIVE=1
        break
    fi
    i=$((i + 2))
    sleep 2
done
[ "$MET_LIVE" -eq 1 ] || {
    msh 'tail -40 /tmp/met-fleetlyd.log' || true
    fatal 'fleetlyd not live within 90s'
}
MET_TOKEN=$(m sh -c 'cat /var/lib/fleetly/bootstrap-token') || fatal 'read bootstrap token'
[ -n "$MET_TOKEN" ] || fatal 'empty bootstrap token'
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
docker exec "$CURLER" curl -s -o /dev/null "http://10.217.0.10:8420/healthz/liveness" ||
    fatal 'curl helper cannot reach the REST face'
docker exec "$CURLER" curl -s -c /tmp/jar -X POST "http://10.217.0.10:8420/v1/auth/register" \
    -H 'Content-Type: application/json' \
    -d '{"email":"founder@e2e.test","password":"founder-pass-1","display_name":"Founder"}' \
    >/dev/null || fatal 'founder register'
# W2-S5 夹具修正：凭据改铸 **machine 令牌**（平台级凭据 = 资源面 admin
# 等价，rbac-teams §2.3；W2-S4 起平台管理员在资源面被 ResolvePermission
# 短路为只读——founder 的用户 PAT 已不能再承担部署/资源写；metrics 模式
# 切换属平台面，机具凭据按 scope 门放行，语义不变）。
MET_TOKEN=$(docker exec "$CURLER" curl -s -b /tmp/jar -X POST "http://10.217.0.10:8420/v1/tokens" \
    -H 'Content-Type: application/json' \
    -d '{"machine":true,"note":"e2e machine token","scopes":["admin"]}' | grep -oE '"token": ?"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "$MET_TOKEN" ] || fatal 'machine token mint failed'
# curl helper 用毕即除（fixture 只承担注册与铸 token；避免钉住 bridge 网络影响后续套件）。
docker rm -f "$CURLER" >/dev/null 2>&1 || true
FOUNDER_TEAM=founder
FOUNDER_PRJ=default
FOUNDER_PROJECT="$FOUNDER_TEAM/$FOUNDER_PRJ"
nl 'founder registered (platform admin); machine token minted; project context '"'"'"$FOUNDER_PROJECT"'"'"''


# ───────── A1: 缺省零常驻（D-W5-2 opt-in——unset 生效，收敛管理器无所欠）
nl '=== A1: default (unset) deploys nothing ==='
nothing_deployed() {
    names=$(m docker service ls --format '{{.Name}}' 2>/dev/null)
    printf '%s' "$names" | grep -q 'fleetly-cadvisor' && return 1
    printf '%s' "$names" | grep -q 'fleetly-node-exporter' && return 1
    printf '%s' "$names" | grep -q 'fleetly-victoriametrics' && return 1
    return 0
}
# 给收敛循环留两拍（30s 退避窗的短侧）确认不是过渡态。
sleep 20
if nothing_deployed; then
    assert "MET-A1 DEFAULT_UNSET_NOTHING_DEPLOYED" 0
else
    m docker service ls || true
    assert "MET-A1 DEFAULT_UNSET_NOTHING_DEPLOYED" 1 "services exist while metrics.mode is unset"
fi

status_unset_ok() {
    fcli metrics status 2>/dev/null | grep -q 'mode: unset'
}
if poll_until 30 status_unset_ok; then
    assert "MET-A2 STATUS_READS_UNSET" 0
else
    fcli metrics status || true
    assert "MET-A2 STATUS_READS_UNSET" 1 "metrics status does not read unset"
fi

# ───────── A3: mode set on（opt-in 置位；受理即返回，收敛异步）
# set 是幂等 upsert——启动拍若与其他收敛循环的库写并发冲突（busy）可重试。
nl '=== A3: metrics mode set on ==='
mode_set_ok() {
    fcli metrics mode set on >/dev/null 2>&1
}
if poll_until 60 mode_set_ok; then
    assert "MET-A3 MODE_SET_ACCEPTED" 0
else
    fcli metrics mode set on || true
    assert "MET-A3 MODE_SET_ACCEPTED" 1 "metrics mode set on never accepted"
fi

# ───────── A4: 三件收敛（首跑已预拉镜像——重试窗内纯调度）
nl '=== A4: three managed services converge ==='
all_running() {
    for svc in fleetly-cadvisor fleetly-node-exporter fleetly-victoriametrics; do
        msh "docker service ps $svc --format '{{.CurrentState}}' | grep -q '^Running'" 2>/dev/null || return 1
    done
    return 0
}
if poll_until 240 all_running; then
    assert "MET-A4 THREE_SERVICES_RUNNING" 0
else
    m docker service ls || true
    m docker service ps fleetly-victoriametrics --no-trunc || true
    assert "MET-A4 THREE_SERVICES_RUNNING" 1 "managed stack never fully ran within 240s"
fi

if poll_until 60 events_grep metrics.stack_deployed; then
    assert "MET-A5 DEPLOYED_EVENT" 0
else
    assert "MET-A5 DEPLOYED_EVENT" 1 "metrics.stack_deployed missing from event stream"
fi

# ───────── A6: VM 回环健康（host 网络任务自绑 127.0.0.1——零公网面）
vm_health_ok() {
    msh 'wget -q -T 3 -O - http://127.0.0.1:8428/health | grep -q OK' 2>/dev/null
}
if poll_until 60 vm_health_ok; then
    assert "MET-A6 VM_LOOPBACK_HEALTH_OK" 0
else
    msh 'wget -S -T 3 -O - http://127.0.0.1:8428/health' || true
    assert "MET-A6 VM_LOOPBACK_HEALTH_OK" 1 "VM /health unreachable on 127.0.0.1:8428"
fi

# ───────── A7: 采集器在位（0.0.0.0 绑定含回环——127.0.0.1 /metrics 冒烟）
collectors_ok() {
    msh 'wget -q -T 3 -O /dev/null http://127.0.0.1:8080/metrics' 2>/dev/null &&
        msh 'wget -q -T 3 -O /dev/null http://127.0.0.1:9100/metrics' 2>/dev/null
}
if poll_until 60 collectors_ok; then
    assert "MET-A7 COLLECTORS_LOOPBACK_METRICS" 0
else
    msh 'wget -S -T 3 -O /dev/null http://127.0.0.1:8080/metrics' || true
    msh 'wget -S -T 3 -O /dev/null http://127.0.0.1:9100/metrics' || true
    assert "MET-A7 COLLECTORS_LOOPBACK_METRICS" 1 "collector /metrics not reachable on loopback"
fi

# ───────── A8: VM 抓取收敛（nodes_reporting 1/1——up 计数诚实口径）
nl '=== A8: VM scraping converges (nodes_reporting 1/1) ==='
nodes_reporting_ok() {
    fcli metrics status 2>/dev/null | grep -q 'nodes_reporting: 1/1'
}
if poll_until 120 nodes_reporting_ok; then
    assert "MET-A8 NODES_REPORTING_1_OF_1" 0
else
    fcli metrics status || true
    assert "MET-A8 NODES_REPORTING_1_OF_1" 1 "VM never reported a converged cAdvisor target"
fi

# ───────── A9: 查询面（PromQL 透传）：up 序列双 job + 节点/容器维度
# （CLI 约定：旗标在位置参数前；query_range 数据在首批抓取后 ~1 分钟内
# 才可见——与 A8 的瞬时计数不同，区间面带重试窗；up 实例 = 节点 advertise
# 地址目标——§6 挂账票修订后的动态 targets 形态）
nl '=== A9: SearchMetrics via CLI hits both scrape jobs ==='
up_both_ok() {
    fcli metrics query --json 'up' 2>/dev/null | grep -q 'fleetly-cadvisor' &&
        fcli metrics query --json 'up' 2>/dev/null | grep -q 'fleetly-node-exporter'
}
if poll_until 120 up_both_ok; then
    assert "MET-A9 UP_SERIES_BOTH_JOBS" 0
else
    fcli metrics query --json 'up' || true
    assert "MET-A9 UP_SERIES_BOTH_JOBS" 1 "up series never covered both jobs"
fi

node_series_ok() {
    fcli metrics query --json 'node_load1' 2>/dev/null | grep -q '"__name__": "node_load1"'
}
if poll_until 120 node_series_ok; then
    assert "MET-A10 NODE_DIMENSION_SERIES" 0
else
    fcli metrics query --json 'node_load1' || true
    assert "MET-A10 NODE_DIMENSION_SERIES" 1 "node dimension series never appeared"
fi

# ───────── A20–A22: §6 挂账票修订面（绑定 0.0.0.0 / advertise 直连 /
# 动态 targets——设计 docs/design/2026-09-22-observability.md §6 挂账票）
nl '=== A20-A22: revised bind face + node-addr scrape targets ==='
# manager advertise 地址（swarm init --advertise-addr eth0 → 10.217.0.10）。
MET_MGR_ADDR=$(m docker node inspect self --format '{{.Status.Addr}}')
[ -n "$MET_MGR_ADDR" ] || fatal 'empty manager advertise addr'

# A20 服务 spec 绑定面：采集器 0.0.0.0（跨节点抓取面）+ VM 恒回环
#（查询面零公网面——绑定面分野双向钉死）。
bind_face_ok() {
    msh "docker service inspect fleetly-cadvisor --format '{{.Spec.TaskTemplate.ContainerSpec.Args}}'" |
        grep -F -q -- '-listen_ip=0.0.0.0' &&
        msh "docker service inspect fleetly-node-exporter --format '{{.Spec.TaskTemplate.ContainerSpec.Args}}'" |
            grep -F -q -- '--web.listen-address=0.0.0.0:9100' &&
        msh "docker service inspect fleetly-victoriametrics --format '{{.Spec.TaskTemplate.ContainerSpec.Args}}'" |
            grep -F -q -- '-httpListenAddr=127.0.0.1:8428'
}
if poll_until 30 bind_face_ok; then
    assert "MET-A20 COLLECTOR_BIND_ALL_INTERFACES" 0
else
    msh "docker service inspect fleetly-cadvisor --format '{{.Spec.TaskTemplate.ContainerSpec.Args}}'" || true
    msh "docker service inspect fleetly-node-exporter --format '{{.Spec.TaskTemplate.ContainerSpec.Args}}'" || true
    msh "docker service inspect fleetly-victoriametrics --format '{{.Spec.TaskTemplate.ContainerSpec.Args}}'" || true
    assert "MET-A20 COLLECTOR_BIND_ALL_INTERFACES" 1 "collector/VM bind face args drifted from the revised topology"
fi

# A21 经节点 advertise 地址直连采集器（manager → 节点地址 = 跨节点抓取
# 路径的单机缩影；宿主命名空间 wget 节点 IP:8080/9100）。
node_addr_reachable_ok() {
    msh "wget -q -T 3 -O /dev/null http://$MET_MGR_ADDR:8080/metrics" 2>/dev/null &&
        msh "wget -q -T 3 -O /dev/null http://$MET_MGR_ADDR:9100/metrics" 2>/dev/null
}
if poll_until 60 node_addr_reachable_ok; then
    assert "MET-A21 COLLECTORS_NODE_ADDR_REACHABLE" 0
else
    msh "wget -S -T 3 -O /dev/null http://$MET_MGR_ADDR:8080/metrics" || true
    msh "wget -S -T 3 -O /dev/null http://$MET_MGR_ADDR:9100/metrics" || true
    assert "MET-A21 COLLECTORS_NODE_ADDR_REACHABLE" 1 "collector /metrics not reachable via the node advertise addr"
fi

# A22 动态 targets：抓取 config 对象内容含 advertise 地址 targets（内容
# 寻址对象名 fleetly-vm-scrape-<sha8>。内容提取 = {{printf "%s" .Spec.Data}}
# ——docker 29 CLI 的裸 {{.Spec.Data}} 把 []byte 打成十进制字节表，base64
# 直解不通，printf %s 直出原文，2026-09-23 宿主实测）。
scrape_config_targets_ok() {
    name=$(msh "docker config ls --format '{{.Name}}'" | grep '^fleetly-vm-scrape-' | head -n1)
    [ -n "$name" ] || return 1
    content=$(msh "docker config inspect --format '{{printf \"%s\" .Spec.Data}}' '$name'")
    [ -n "$content" ] || return 1
    printf '%s' "$content" | grep -F -q "$MET_MGR_ADDR:8080" &&
        printf '%s' "$content" | grep -F -q "$MET_MGR_ADDR:9100"
}
if poll_until 60 scrape_config_targets_ok; then
    assert "MET-A22 SCRAPE_CONFIG_NODE_ADDR_TARGETS" 0
else
    msh "docker config ls" || true
    msh "docker config inspect --format '{{printf \"%s\" .Spec.Data}}' \$(docker config ls --format '{{.Name}}' | grep '^fleetly-vm-scrape-' | head -n1)" || true
    assert "MET-A22 SCRAPE_CONFIG_NODE_ADDR_TARGETS" 1 "scrape config content never carried the advertise-addr targets"
fi

# 部署 whoami 应用产生真实容器指标（应用归属标签实测形态的载体）。
nl '=== A11: app deploy + container dimension series ==='
cat >"$TMP/app-compose.yaml" <<EOF
name: $APP
services:
  $SVC:
    image: $WHOAMI_IMG
    deploy:
      replicas: 2
    expose: ["80"]
    healthcheck:
      test: ["NONE"]
EOF
stage "$DIND" "$TMP/app-compose.yaml" /opt/fleetly/met-compose.yaml
docker exec -d -e FLEETLY_ADDR=127.0.0.1:8421 -e FLEETLY_TOKEN="$MET_TOKEN" -e FLEETLY_PROJECT="$FOUNDER_PROJECT" \
    "$DIND" sh -c '/opt/fleetly/bin/fleetly deploy --timeout 300s /opt/fleetly/met-compose.yaml > /tmp/met-deploy.log 2>&1'
deploy_succeeded() {
    fcli deployments list --json "$APP" 2>/dev/null | grep -q '"status": "succeeded"'
}
if poll_until 300 deploy_succeeded; then
    assert "MET-A11a APP_DEPLOY_SUCCEEDED" 0
else
    fcli deployments list --json "$APP" 2>/dev/null || true
    msh 'cat /tmp/met-deploy.log' || true
    assert "MET-A11a APP_DEPLOY_SUCCEEDED" 1 "deployment never succeeded"
fi

container_series_ok() {
    fcli metrics query --json 'container_memory_usage_bytes' 2>/dev/null |
        grep -q '"__name__": "container_memory_usage_bytes"'
}
if poll_until 120 container_series_ok; then
    assert "MET-A12 CONTAINER_DIMENSION_SERIES" 0
else
    fcli metrics query --json 'container_memory_usage_bytes' || true
    assert "MET-A12 CONTAINER_DIMENSION_SERIES" 1 "container dimension series never appeared"
fi

# 应用归属标签实测形态（探针而非硬断言——真实形态如实记录进套件日志）：
# 平台命名公式 = `fleetly-<app>-<svc>`（internal/naming.ServiceName 同源）。
# **已知上游限制（2026-09-22 实测）**：docker 29 containerd-snapshotter 形态
# 下 cAdvisor 的 containerd 元数据只带 engine bundle-path 扩展，swarm 归属
# 标签缺席——命中则记 LABEL-HIT，缺席则核验容器序列仍在并记 LABEL-ABSENT
# （Console 按 label 分组呈诚实空态 + 降级说明；恢复随 cAdvisor 上游修复）。
nl '=== A13: app attribution label form probe (honest) ==='
label_hit() {
    fcli metrics query --json 'container_memory_usage_bytes{container_label_com_docker_swarm_service_name=~"^fleetly-founder-default-metricsapp-.*"}' 2>/dev/null |
        grep -q "fleetly-$FOUNDER_TEAM-$FOUNDER_PRJ-metricsapp-web"
}
if label_hit; then
    nl "MET-A13 SWARM_SERVICE_LABEL: LABEL-HIT (fleetly-metricsapp-web observed)"
    assert "MET-A13 SWARM_SERVICE_LABEL_FORM" 0
else
    per_container_ok() {
        fcli metrics query --json 'container_memory_usage_bytes' 2>/dev/null |
            grep -q '"/docker/'
    }
    if poll_until 60 per_container_ok; then
        nl "MET-A13 SWARM_SERVICE_LABEL: LABEL-ABSENT (snapshotter mode; per-container series still collected — documented upstream limitation)"
        assert "MET-A13 SWARM_SERVICE_LABEL_FORM" 0
    else
        fcli metrics query --json 'container_memory_usage_bytes' || true
        assert "MET-A13 SWARM_SERVICE_LABEL_FORM" 1 "neither swarm labels nor per-container series observed"
    fi
fi

# ───────── A14: CLI 人读形态（旗标在位置参数前）+ status 全景
cli_human_hit() {
    fcli metrics query --since 30m --step 15 'up' 2>/dev/null |
        grep -q 'fleetly-cadvisor'
}
if poll_until 60 cli_human_hit; then
    assert "MET-A14 CLI_HUMAN_QUERY_HIT" 0
else
    fcli metrics query --since 30m --step 15 'up' || true
    assert "MET-A14 CLI_HUMAN_QUERY_HIT" 1 "human query never hit"
fi

status_on_ok() {
    fcli metrics status 2>/dev/null | grep -q 'mode: on' &&
        fcli metrics status 2>/dev/null | grep -q 'component fleetly-cadvisor: deployed' &&
        fcli metrics status 2>/dev/null | grep -q 'component fleetly-node-exporter: deployed' &&
        fcli metrics status 2>/dev/null | grep -q 'component fleetly-victoriametrics: deployed' &&
        fcli metrics status 2>/dev/null | grep -q 'nodes_reporting: 1/1'
}
if poll_until 30 status_on_ok; then
    assert "MET-A15 STATUS_ON_FULL_VIEW" 0
else
    fcli metrics status || true
    assert "MET-A15 STATUS_ON_FULL_VIEW" 1 "metrics status not fully green while on"
fi

# ───────── A23: `metrics status` 新诚实 note（on 态常驻——采集面 = 内网
# 面，公网访问由节点防火墙负责；spec 注释 / status note / Console 文案
# 三处同锚。单节点 1/1 下缺席分支 note 不出现——文案分支的另一面）。
exposure_note_ok() {
    fcli metrics status 2>/dev/null | grep -q 'note: collector ports listen on all node interfaces' &&
        fcli metrics status 2>/dev/null | grep -q 'blocked by the node firewall'
}
if poll_until 30 exposure_note_ok; then
    assert "MET-A23 STATUS_EXPOSURE_NOTE" 0
else
    fcli metrics status || true
    assert "MET-A23 STATUS_EXPOSURE_NOTE" 1 "the honest exposure note is missing from metrics status (on)"
fi

# ───────── A16: mode set unset（三件移除 + 卷保留 + removed 事件）
nl '=== A16: mode set unset removes the stack, keeps the volume ==='
mode_unset_ok() {
    fcli metrics mode set unset >/dev/null 2>&1
}
if ! poll_until 60 mode_unset_ok; then
    fcli metrics mode set unset || true
    fatal 'metrics mode set unset never accepted'
fi
all_gone() {
    names=$(m docker service ls --format '{{.Name}}' 2>/dev/null)
    printf '%s' "$names" | grep -q 'fleetly-cadvisor' && return 1
    printf '%s' "$names" | grep -q 'fleetly-node-exporter' && return 1
    printf '%s' "$names" | grep -q 'fleetly-victoriametrics' && return 1
    return 0
}
if poll_until 180 all_gone; then
    assert "MET-A16 SERVICES_REMOVED_ON_UNSET" 0
else
    m docker service ls || true
    assert "MET-A16 SERVICES_REMOVED_ON_UNSET" 1 "services still present after set unset"
fi

vol_kept() {
    msh "docker volume ls -q | grep -q '^fleetly-victoriametrics-data$'" 2>/dev/null
}
if poll_until 30 vol_kept; then
    assert "MET-A17 VOLUME_RETAINED_ON_UNSET" 0
else
    m docker volume ls || true
    assert "MET-A17 VOLUME_RETAINED_ON_UNSET" 1 "fleetly-victoriametrics-data must survive the switch"
fi

if poll_until 60 events_grep metrics.stack_removed; then
    assert "MET-A18 REMOVED_EVENT" 0
else
    assert "MET-A18 REMOVED_EVENT" 1 "metrics.stack_removed missing from event stream"
fi

# ───────── A19: 关闭后查询诚实报错（不返回空序列冒充有数）
nl '=== A19: query after disable is honest (E_METRICS_NOT_ENABLED) ==='
HONEST_OUT=$(fcli metrics query 'up' 2>&1)
HONEST_RC=$?
if [ "$HONEST_RC" -ne 0 ] && printf '%s' "$HONEST_OUT" | grep -q 'E_METRICS_NOT_ENABLED'; then
    assert "MET-A19 QUERY_HONEST_NOT_ENABLED" 0
else
    assert "MET-A19 QUERY_HONEST_NOT_ENABLED" 1 "rc=$HONEST_OUT body=$HONEST_OUT"
fi

finish
