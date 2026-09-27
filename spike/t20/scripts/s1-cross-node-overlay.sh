#!/bin/sh
# spike/t20/scripts/s1-cross-node-overlay.sh — Spike ① 跨节点腿（双 dind swarm）。
#
# 语义矩阵的跨节点列：overlay internal（被测）与 overlay 非 internal（基线）。
# 每格两个层面：
#   L1 attachable 容器（docker run --network）：mgr/w1 各一容器互测 DNS/ICMP/TCP；
#   L2 swarm service（global，两任务）：tasks.<svc> 跨节点发现 + 逐地址 ICMP。
# 用法（Git Bash，仓库根目录）：sh spike/t20/scripts/s1-cross-node-overlay.sh
#
# 产物：spike/t20/artifacts/<run>/s1-cx-*.log + s1-cx-summary.txt
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

SELF=$(printf '%s' "$0" | tr '\\' '/')
ROOT=$(CDPATH= cd -- "$(dirname -- "$SELF")/../../.." && pwd)
. "$ROOT/spike/t20/scripts/lib.sh"
cd "$ROOT" || t20_die 'cd repo root'

t20_need_docker
t20_init_run_dir s1-cx
MGR=t20-cx-mgr
W1=t20-cx-w1
MGR_IP=10.240.0.10
W1_IP=10.240.0.11
ALPINE='alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc'
SUMMARY="$T20_RUN_DIR/s1-cx-summary.txt"
PROBE="$ROOT/spike/t20/scripts/in-overlay-probe.sh"
SVC_PROBE="$ROOT/spike/t20/scripts/in-cx-service-probe.sh"

say() { printf '%s\n' "$*" | tee -a "$SUMMARY"; }
m() { docker exec "$MGR" "$@"; }
w1() { docker exec "$W1" "$@"; }

trap 't20_teardown' EXIT INT TERM

t20_bridge_up
t20_dind_up "$MGR" "$MGR_IP" mgr
t20_dind_up "$W1" "$W1_IP" w1

say '=== swarm init + join ==='
m docker swarm init --advertise-addr eth0 >>"$T20_RUN_DIR/s1-cx-swarm.log" 2>&1 || t20_die 'swarm init'
TOKEN=$(m docker swarm join-token -q worker) || t20_die 'join token'
w1 docker swarm join "$MGR_IP:2377" --token "$TOKEN" >>"$T20_RUN_DIR/s1-cx-swarm.log" 2>&1 ||
    t20_die 'w1 join'
i=0
while :; do
    ready=$(m sh -c "docker node ls --format '{{.Status}}' | grep -c '^Ready'")
    [ "$ready" -ge 2 ] && break
    i=$((i + 2))
    [ "$i" -ge 90 ] && t20_die 'two nodes not Ready'
    sleep 2
done
say 'two nodes Ready'

# 暂存探针脚本到两个 dind 的 /opt/t20（service bind mount 需要节点本地存在）。
for d in "$MGR" "$W1"; do
    docker exec "$d" mkdir -p /opt/t20
    t20_stage "$d" "$PROBE" /opt/t20/in-overlay-probe.sh
    t20_stage "$d" "$SVC_PROBE" /opt/t20/in-cx-service-probe.sh
done

# run_cell <cell> <create-args...>
run_cell() {
    cell=$1
    shift
    net="t20-cx-$cell"
    log="$T20_RUN_DIR/s1-cx-$cell.log"
    : >"$log"
    say "=== CELL $cell (net=$net args: $*) ==="
    m docker network create "$@" "$net" >>"$log" 2>&1 || {
        say "CELL $cell NETWORK_CREATE=fail"
        return
    }
    # L1：两节点各一 attachable 容器（w1 容器有监听；mgr 容器被探测后回探）。
    # overlay 网络创建后需传播窗口；attach 失败重试（Docker 29 有界重试）。
    listen='while true; do nc -l -p 8080 >/tmp/t20-nc-listener.log 2>&1; done'
    sleep 3
    ok_mgr=1
    for _ in 1 2 3; do
        if m docker run -d --name "t20-cx-$cell-mgr" --network "$net" "$ALPINE" \
            sh -c "$listen" >>"$log" 2>&1; then
            ok_mgr=0
            break
        fi
        m docker rm -f "t20-cx-$cell-mgr" >>"$log" 2>&1 || true
        sleep 3
    done
    [ "$ok_mgr" -eq 0 ] || say "CELL $cell MGR_CTR=fail"
    ok_w1=1
    for _ in 1 2 3; do
        if w1 docker run -d --name "t20-cx-$cell-w1" --network "$net" "$ALPINE" \
            sh -c "$listen" >>"$log" 2>&1; then
            ok_w1=0
            break
        fi
        w1 docker rm -f "t20-cx-$cell-w1" >>"$log" 2>&1 || true
        sleep 3
    done
    [ "$ok_w1" -eq 0 ] || say "CELL $cell W1_CTR=fail"
    t20_exec_stdin "$MGR" docker exec -i "t20-cx-$cell-mgr" sh -c 'cat > /probe.sh' <"$PROBE" \
        >>"$log" 2>&1 || t20_die "stage probe into mgr ctr"
    t20_exec_stdin "$W1" docker exec -i "t20-cx-$cell-w1" sh -c 'cat > /probe.sh' <"$PROBE" \
        >>"$log" 2>&1 || t20_die "stage probe into w1 ctr"
    echo "--- L1 mgr -> w1 ---" >>"$log"
    m docker exec "t20-cx-$cell-mgr" sh /probe.sh "t20-cx-$cell-w1" >>"$log" 2>&1 || true
    echo "--- L1 w1 -> mgr ---" >>"$log"
    w1 docker exec "t20-cx-$cell-w1" sh /probe.sh "t20-cx-$cell-mgr" >>"$log" 2>&1 || true

    # L2：global service 两任务（探针经 bind mount 挂入；探针跑完 sleep 保活，
    # 便于日志收集；--detach 防止 create 等待收敛阻塞）。
    svc="t20-cx-$cell-svc"
    m docker service create --detach --name "$svc" --network "$net" --mode global \
        --restart-condition none \
        --mount "type=bind,source=/opt/t20/in-cx-service-probe.sh,target=/probe.sh,readonly" \
        "$ALPINE" sh -c "sh /probe.sh $svc; sleep 600" >>"$log" 2>&1 ||
        say "CELL $cell SVC_CREATE=fail"
    i=0
    while :; do
        svc_logs=$(m docker service logs "$svc" 2>&1 || true)
        done_n=$(printf '%s\n' "$svc_logs" | grep -c 'SVC_PROBE_END')
        [ "$done_n" -ge 2 ] && break
        i=$((i + 2))
        [ "$i" -ge 180 ] && break
        sleep 2
    done
    {
        echo "--- L2 service ps ---"
        m docker service ps "$svc" --no-trunc
        echo "--- L2 service logs (aggregated by manager) ---"
        m docker service logs "$svc" 2>&1
    } >>"$log" 2>&1
    grep -E '^(DNS_PEER|DNS_PUBLIC|TCP_PEER|TCP_PUBLIC_80|HTTP_PUBLIC|INTERFACES|ROUTE_TABLE)=' "$log" |
        sed "s/^/CELL $cell L1 /" >>"$SUMMARY"
    grep -E '(SVC_TASKS_DNS|SVC_PING_|SVC_TASK_ADDRESSES|DNS_PUBLIC|TCP_PUBLIC_80)=' "$log" |
        sed "s/^/CELL $cell L2 /" >>"$SUMMARY"
    say "CELL $cell DONE (log: $log)"

    m docker service rm "$svc" >/dev/null 2>&1 || true
    m docker rm -f "t20-cx-$cell-mgr" >/dev/null 2>&1 || true
    w1 docker rm -f "t20-cx-$cell-w1" >/dev/null 2>&1 || true
    m docker network rm "$net" >/dev/null 2>&1 || true
}

run_cell ov-in --attachable --internal -d overlay
run_cell ov --attachable -d overlay

say '=== S1-CROSS-NODE DONE ==='
