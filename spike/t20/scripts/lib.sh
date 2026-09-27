#!/bin/sh
# spike/t20/scripts/lib.sh — IMPL-T2-0 探针公共库（宿主侧 Git Bash 编排）。
#
# 用法（从仓库根目录）：
#   . spike/t20/scripts/lib.sh
#   t20_init_run_dir s1
#   t20_dind_up t20-mgr 10.240.0.10 mgr
#
# 纪律（本目录所有脚本）：
#   - 禁「stderr 丢弃到 /dev/null」写法（stderr 一律进日志文件或变量）；
#   - 全部脚本过 `sh -n`；
#   - 远程/dind 内复杂操作 = 宿主写脚本 → exec+stdin 送入 → sh 执行；
#   - 产物落 spike/t20/artifacts/<run-id>/（随报告入库）。
set -u

T20_ROOT=${T20_ROOT:-spike/t20}
T20_ART_ROOT=${T20_ART_ROOT:-$T20_ROOT/artifacts}
T20_BR_NET=${T20_BR_NET:-t20-br}
T20_BR_SUBNET=${T20_BR_SUBNET:-10.240.0.0/24}
T20_DIND_IMAGE=${T20_DIND_IMAGE:-docker:29.8.1-dind@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0}
T20_DIND_NAMES=${T20_DIND_NAMES:-}

t20_log() {
    printf '[t20 %s] %s\n' "$(date -u +%H:%M:%S)" "$*"
}

t20_die() {
    printf '[t20 %s] FATAL %s\n' "$(date -u +%H:%M:%S)" "$*" >&2
    exit 1
}

# t20_init_run_dir <tag> — 建一轮产物目录并回显路径（T20_RUN_DIR）。
t20_init_run_dir() {
    tag=${1:-run}
    T20_RUN_DIR=${T20_RUN_DIR:-$T20_ART_ROOT/$(date -u +%Y%m%d-%H%M%S)-$tag}
    mkdir -p "$T20_RUN_DIR" || t20_die "mkdir $T20_RUN_DIR"
    export T20_RUN_DIR
    t20_log "run dir: $T20_RUN_DIR"
}

# t20_need_docker — 宿主 docker 可用性门禁（含 swarm 状态回显）。
t20_need_docker() {
    command -v docker >/dev/null 2>&1 || t20_die 'docker not on PATH'
    t20_log "host engine: $(docker version --format '{{.Server.Version}}') swarm=$(docker info --format '{{.Swarm.LocalNodeState}}')"
}

# t20_dind_up <name> <ip> <node-hostname> [extra dockerd args...] — 起一个特权 dind。
t20_dind_up() {
    name=$1
    ip=$2
    hostname=$3
    shift 3
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker run -d --name "$name" --privileged --hostname "$hostname" \
        --network "$T20_BR_NET" --ip "$ip" "$T20_DIND_IMAGE" "$@" >/dev/null ||
        t20_die "docker run $name"
    T20_DIND_NAMES="$T20_DIND_NAMES $name"
    export T20_DIND_NAMES
    i=0
    while ! docker exec "$name" docker info >/dev/null 2>&1; do
        i=$((i + 2))
        if [ "$i" -ge 90 ]; then
            docker logs "$name" --tail 40 || true
            t20_die "inner dockerd of $name not ready within 90s"
        fi
        sleep 2
    done
    t20_log "dind $name ready (engine $(docker exec "$name" docker version --format '{{.Server.Version}}'))"
}

# t20_stage <dind> <host-file> <remote-path> — exec+stdin 直传 + 体积校验 + CR 剥离。
t20_stage() {
    d=$1
    f=$2
    r=$3
    docker exec -i "$d" sh -c "cat > '$r'" <"$f" || t20_die "stage $f -> $d:$r"
    hsz=$(wc -c <"$f" | tr -d ' ')
    gsz=$(docker exec "$d" sh -c "wc -c < '$r'" | tr -d ' ')
    [ "$hsz" = "$gsz" ] || t20_die "size mismatch for $r: host=$hsz dind=$gsz"
    case "$r" in
    *.sh)
        docker exec "$d" sed -i 's/\r$//' "$r" || t20_die "strip CR from $r"
        ;;
    esac
}

# t20_teardown — 拆除本轮 dind 与宿主 bridge 网络（幂等）。
t20_teardown() {
    for d in $T20_DIND_NAMES; do
        docker rm -f "$d" >/dev/null 2>&1 || true
    done
    T20_DIND_NAMES=''
    export T20_DIND_NAMES
    docker network rm "$T20_BR_NET" >/dev/null 2>&1 || true
}

# t20_bridge_up — 建 dind 互联 bridge 网络（幂等重建）。
t20_bridge_up() {
    leftover=$(docker ps -aq --filter name=t20- 2>&1 || true)
    if [ -n "$leftover" ]; then
        t20_log "WARN removing leftover t20 containers: $leftover"
        echo "$leftover" | xargs docker rm -f >/dev/null 2>&1 || true
    fi
    docker network rm "$T20_BR_NET" >/dev/null 2>&1 || true
    docker network create -d bridge --subnet "$T20_BR_SUBNET" "$T20_BR_NET" >/dev/null ||
        t20_die "create $T20_BR_NET"
}

# t20_exec_stdin <dind> <inner-cmd...> — 嵌套 docker exec 的 stdin 直传
# （外层必须 -i，否则内层 docker exec -i 拿到空 stdin）。
t20_exec_stdin() {
    d=$1
    shift
    docker exec -i "$d" "$@"
}

# t20_wait_alive <dind> <log-path> — 轮询 inner dockerd（docker exec 可用即真）。
t20_wait_alive() {
    d=$1
    i=0
    while ! docker exec "$d" docker info >/dev/null 2>&1; do
        i=$((i + 2))
        [ "$i" -ge 120 ] && return 1
        sleep 2
    done
    return 0
}
