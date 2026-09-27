#!/bin/sh
# spike/t20/scripts/in-lifecycle-latency.sh — Spike ③ 内层计时（dind 内运行）。
#
# 用法（单 dind 内）：sh in-lifecycle-latency.sh [N]
#   Arm A：swarm service 周期 = create → task running → healthy → update --force
#          → 新任务 healthy → rm → 任务消失；
#   Arm B：docker run/stop 直操基线（torchwood dispatcher 现役形态等价物）。
# 计时全部取 docker events 的 TimeNano（同 daemon 时钟），不取 poll 墙钟。
# 输出：每周期 CYCLE/<arm>_<metric>_ms=<n> 行 + EVENTS_FILE 路径。
set -u

N=${1:-30}
IMG='alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc'
EV=/tmp/t20-s3-events.log
# 有界停机宽限：sleep 作为 PID 1 忽略 SIGTERM → 默认 10s 宽限会主导
# update/rm 时延（默认宽限对照见首轮 run，本脚本用 2s 隔离控制面时延）。
STOP_GRACE=${T20_S3_STOP_GRACE:-2}
HEALTH_ARGS='--health-cmd true --health-interval 1s --health-timeout 1s --health-retries 1 --health-start-period 0s'

say() { printf '%s\n' "$*"; }
now_ms() { date +%s%3N 2>&1 || date +%s; }

say "INNER_ENGINE=$(docker version --format '{{.Server.Version}}')"
say "N=$N STOP_GRACE=${STOP_GRACE}s"

docker swarm init --advertise-addr eth0 >/tmp/t20-s3-init.log 2>&1 || {
    say "SWARM_INIT=fail"
    exit 1
}
say 'SWARM_INIT=ok'

# 预拉镜像（消除拉取噪声；digest 已在节点本地）。
docker pull -q "$IMG" >/tmp/t20-s3-pull.log 2>&1 || say 'PREPULL=fail'

# ── 事件采集（整轮单流；TimeNano 纳秒精度）
docker events --format '{{.TimeNano}}|{{.Type}}|{{.Action}}|{{.Actor.ID}}|{{.Actor.Attributes.name}}' >"$EV" 2>&1 &
EVPID=$!
say "EVENTS_PID=$EVPID"
sleep 1

wait_health() { # <ctr-id> <want> <cap-s>
    cid=$1
    want=$2
    cap=$3
    wh_i=0
    while [ "$wh_i" -lt "$cap" ]; do
        st=$(docker inspect "$cid" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>&1) || st='gone'
        [ "$st" = "$want" ] && return 0
        wh_i=$((wh_i + 1))
        sleep 1
    done
    return 1
}

ctr_of() { # <svc-name> — 该服务当前唯一任务容器 ID（start-first 窗口取最新）
    docker ps -q --filter "label=com.docker.swarm.service.name=$1" 2>&1 | head -1
}

# ── Arm A：swarm service 周期
say '=== ARM A: swarm service lifecycle ==='
cycle_i=1
while [ "$cycle_i" -le "$N" ]; do
    svc="t20-lat-svc-$cycle_i"
    docker service create --detach --name "$svc" --restart-condition none \
        --stop-grace-period "${STOP_GRACE}s" $HEALTH_ARGS \
        "$IMG" sleep 300 >/tmp/t20-s3-create.log 2>&1 || say "A create_fail=$cycle_i"
    cid=''
    wait_k=0
    while [ "$wait_k" -lt 60 ]; do
        cid=$(ctr_of "$svc")
        [ -n "$cid" ] && break
        wait_k=$((wait_k + 1))
        sleep 1
    done
    if [ -z "$cid" ]; then
        say "A task_missing=$cycle_i"
        docker service rm "$svc" >/tmp/t20-s3-rm.log 2>&1 || true
        cycle_i=$((cycle_i + 1))
        continue
    fi
    wait_health "$cid" healthy 60 || say "A healthy_timeout=$cycle_i"
    oldcid=$cid
    docker service update --force -d "$svc" >/tmp/t20-s3-update.log 2>&1 || say "A update_fail=$cycle_i"
    # 等新容器出现且 healthy（stop-first：旧容器退场后新容器接棒）。
    newcid=''
    wait_k=0
    while [ "$wait_k" -lt 90 ]; do
        newcid=$(ctr_of "$svc")
        if [ -n "$newcid" ] && [ "$newcid" != "$oldcid" ]; then
            st=$(docker inspect "$newcid" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>&1) || st='gone'
            [ "$st" = healthy ] && break
        fi
        wait_k=$((wait_k + 1))
        sleep 1
    done
    [ -n "$newcid" ] && [ "$newcid" != "$oldcid" ] || say "A update_task_timeout=$cycle_i"
    rm_at=$(now_ms)
    docker service rm "$svc" >/tmp/t20-s3-rm.log 2>&1 || say "A rm_fail=$cycle_i"
    wait_k=0
    while [ "$wait_k" -lt 60 ]; do
        left=$(docker ps -q --filter "label=com.docker.swarm.service.name=$svc" 2>&1 | wc -l | tr -d ' ')
        [ "$left" -eq 0 ] && break
        wait_k=$((wait_k + 1))
        sleep 1
    done
    [ "$left" -eq 0 ] || say "A rm_residue=$cycle_i left=$left"
    say "A cycle_done=$cycle_i rm_wall_ms=$(( $(now_ms) - rm_at ))"
    cycle_i=$((cycle_i + 1))
done

# ── Arm B：docker run/stop 直操基线
say '=== ARM B: docker run/stop baseline ==='
cycle_j=1
while [ "$cycle_j" -le "$N" ]; do
    ctr="t20-lat-ctr-$cycle_j"
    docker run -d --name "$ctr" --stop-timeout "$STOP_GRACE" $HEALTH_ARGS "$IMG" sleep 300 >/tmp/t20-s3-run.log 2>&1 ||
        say "B run_fail=$cycle_j"
    wait_health "$ctr" healthy 30 || say "B healthy_timeout=$cycle_j"
    docker stop -t "$STOP_GRACE" "$ctr" >/tmp/t20-s3-stop.log 2>&1 || say "B stop_fail=$cycle_j"
    wait_k=0
    while [ "$wait_k" -lt 30 ]; do
        st=$(docker inspect "$ctr" --format '{{.State.Status}}' 2>&1) || st='gone'
        [ "$st" = 'exited' ] && break
        wait_k=$((wait_k + 1))
        sleep 1
    done
    docker rm -f "$ctr" >/tmp/t20-s3-rmctr.log 2>&1 || true
    say "B cycle_done=$cycle_j"
    cycle_j=$((cycle_j + 1))
done

sleep 3
kill "$EVPID" >/dev/null 2>&1 || true
sleep 1

# ── 解析：按事件流计算每周期时延（ms）。
awk -F'|' '
function delta(a, b) { return (b - a) / 1000000.0 }
{
    ts = $1; type = $2; action = $3; name = $5
    if (type == "service" && action == "create" && name ~ /^t20-lat-svc-/) {
        idx = name; svc_create[idx] = ts; svc_order[++svc_n] = idx
    }
    if (type == "service" && action == "update" && name ~ /^t20-lat-svc-/) {
        svc_update[name] = ts
    }
    if (type == "service" && action == "remove" && name ~ /^t20-lat-svc-/) {
        svc_remove[name] = ts
    }
    if (type == "container") {
        if (name ~ /^t20-lat-svc-/) {
            split(name, parts, ".")
            svc = parts[1]
            if (action == "start") { cstart[svc] = cstart[svc] " " ts; cstart_n[svc]++ }
            if (action == "health_status: healthy") { chealthy[svc] = chealthy[svc] " " ts; chealthy_n[svc]++ }
            if (action == "destroy") { cdestroy[svc] = ts; cdestroy_n[svc]++ }
            if (action == "die") { cdie[svc] = ts; cdie_n[svc]++ }
        }
        if (name ~ /^t20-lat-ctr-/) {
            if (action == "create") { b_create[name] = ts }
            if (action == "start") { b_start[name] = ts }
            if (action == "health_status: healthy") { b_healthy[name] = ts }
            if (action == "die") { b_die[name] = ts }
            if (action == "destroy") { b_destroy[name] = ts }
        }
    }
}
END {
    printf "--- ARM A per-cycle metrics (ms) ---\n"
    for (k = 1; k <= svc_n; k++) {
        s = svc_order[k]
        n1 = split(cstart[s], a1, " "); n2 = split(chealthy[s], a2, " ")
        base = svc_create[s]
        if (base == "") continue
        if (n1 >= 1 && a1[1] != "") printf "A create_to_running_ms=%d svc=%s\n", delta(base, a1[1]), s
        if (n2 >= 1 && a2[1] != "") printf "A create_to_healthy_ms=%d svc=%s\n", delta(base, a2[1]), s
        if (svc_update[s] != "" && n1 >= 2 && a1[2] != "") printf "A update_to_running_ms=%d svc=%s\n", delta(svc_update[s], a1[2]), s
        if (svc_update[s] != "" && n2 >= 2 && a2[2] != "") printf "A update_to_healthy_ms=%d svc=%s\n", delta(svc_update[s], a2[2]), s
        if (svc_remove[s] != "" && cdestroy[s] != "") printf "A rm_to_destroy_ms=%d svc=%s\n", delta(svc_remove[s], cdestroy[s]), s
        if (svc_remove[s] != "" && cdie[s] != "") printf "A rm_to_die_ms=%d svc=%s\n", delta(svc_remove[s], cdie[s]), s
    }
    printf "--- ARM B per-cycle metrics (ms) ---\n"
    for (name in b_create) {
        if (b_start[name] != "") printf "B create_to_running_ms=%d ctr=%s\n", delta(b_create[name], b_start[name]), name
        if (b_healthy[name] != "") printf "B create_to_healthy_ms=%d ctr=%s\n", delta(b_create[name], b_healthy[name]), name
        if (b_die[name] != "") printf "B run_to_die_ms=%d ctr=%s\n", delta(b_create[name], b_die[name]), name
        if (b_start[name] != "" && b_die[name] != "") printf "B start_to_die_ms=%d ctr=%s\n", delta(b_start[name], b_die[name]), name
    }
}
' "$EV" >/tmp/t20-s3-metrics.txt

say "METRICS_FILE=/tmp/t20-s3-metrics.txt"
cat /tmp/t20-s3-metrics.txt
say 'INNER_DONE'
