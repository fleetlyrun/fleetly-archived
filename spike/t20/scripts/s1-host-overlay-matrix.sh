#!/bin/sh
# spike/t20/scripts/s1-host-overlay-matrix.sh — Spike ① 单节点矩阵（宿主 swarm）。
#
# Docker 29 internal overlay 出网实证（DT-7）：四格矩阵 × 出网/DNS/对等面。
#   br     = bridge（非 internal 基线）
#   br-in  = bridge --internal（对照组：无 NAT 全 deny）
#   ov     = overlay --attachable（非 internal 基线）
#   ov-in  = overlay --attachable --internal（被测）
# 用法（Git Bash，仓库根目录）：sh spike/t20/scripts/s1-host-overlay-matrix.sh
#
# 产物：spike/t20/artifacts/<run>/s1-host-*.log + s1-host-summary.txt
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

SELF=$(printf '%s' "$0" | tr '\\' '/')
ROOT=$(CDPATH= cd -- "$(dirname -- "$SELF")/../../.." && pwd)
. "$ROOT/spike/t20/scripts/lib.sh"
cd "$ROOT" || t20_die 'cd repo root'

t20_need_docker
t20_init_run_dir s1-host
PROBE_SRC="$ROOT/spike/t20/scripts/in-overlay-probe.sh"
ALPINE='alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc'
SUMMARY="$T20_RUN_DIR/s1-host-summary.txt"

say() { printf '%s\n' "$*" | tee -a "$SUMMARY"; }

cleanup() {
    for c in br br-in ov ov-in; do
        docker rm -f "t20-s1-$c-probe" "t20-s1-$c-peer" >/dev/null 2>&1 || true
        docker network rm "t20-s1-$c" >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT INT TERM
cleanup

# run_cell <cell> <network-create-args...>
run_cell() {
    cell=$1
    shift
    net="t20-s1-$cell"
    probe="t20-s1-$cell-probe"
    peer="t20-s1-$cell-peer"
    log="$T20_RUN_DIR/s1-host-$cell.log"
    : >"$log"
    say "=== CELL $cell (net=$net args: $*) ==="

    if ! docker network create "$@" "$net" >>"$log" 2>&1; then
        say "CELL $cell NETWORK_CREATE=fail"
        return
    fi
    # 对等容器：busybox nc 长活监听 8080（逐连接循环）。
    if ! docker run -d --name "$peer" --network "$net" "$ALPINE" \
        sh -c 'while true; do nc -l -p 8080 >/tmp/t20-nc-listener.log 2>&1; done' >>"$log" 2>&1; then
        say "CELL $cell PEER_RUN=fail"
        return
    fi
    if ! docker run -d --name "$probe" --network "$net" "$ALPINE" sleep 600 >>"$log" 2>&1; then
        say "CELL $cell PROBE_RUN=fail"
        return
    fi
    docker exec -i "$probe" sh -c 'cat > /probe.sh' <"$PROBE_SRC" >>"$log" 2>&1 ||
        t20_die "stage probe into $probe"
    docker exec "$probe" sh /probe.sh "$peer" >>"$log" 2>&1 || true
    {
        echo "--- probe container inspect (network attrs) ---"
        docker inspect "$probe" --format '{{json .NetworkSettings.Networks}}'
        echo "--- peer log tail ---"
        docker exec "$peer" tail -5 /tmp/t20-nc-listener.log 2>&1 || true
    } >>"$log" 2>&1
    # 汇总关键行
    grep -E '^(DNS_PEER|DNS_PUBLIC|TCP_PEER|TCP_PUBLIC_80|TCP_PUBLIC_53|HTTP_PUBLIC|INTERFACES|ROUTE_TABLE)=' "$log" |
        sed "s/^/CELL $cell /" >>"$SUMMARY"
    say "CELL $cell DONE (log: $log)"

    docker rm -f "$probe" "$peer" >/dev/null 2>&1 || true
    docker network rm "$net" >/dev/null 2>&1 || true
}

run_cell br -d bridge
run_cell br-in -d bridge --internal
run_cell ov --attachable -d overlay
run_cell ov-in --attachable --internal -d overlay

say '=== S1-HOST DONE ==='
