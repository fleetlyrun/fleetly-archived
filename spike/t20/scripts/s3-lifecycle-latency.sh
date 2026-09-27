#!/bin/sh
# spike/t20/scripts/s3-lifecycle-latency.sh — Spike ③ 宿主编排（单 dind swarm）。
#
# swarm service 生命周期时延基线 ×N（默认 30）+ docker run/stop 直操基线；
# 分布统计（min/p50/p90/max）由宿主侧汇总。用法：
#   sh spike/t20/scripts/s3-lifecycle-latency.sh [N]
# 产物：spike/t20/artifacts/<run>/s3-*.log/txt
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

SELF=$(printf '%s' "$0" | tr '\\' '/')
ROOT=$(CDPATH= cd -- "$(dirname -- "$SELF")/../../.." && pwd)
. "$ROOT/spike/t20/scripts/lib.sh"
cd "$ROOT" || t20_die 'cd repo root'

N=${1:-${T20_S3_N:-30}}
t20_need_docker
t20_init_run_dir s3
DIND=t20-s3
INNER="$ROOT/spike/t20/scripts/in-lifecycle-latency.sh"
OUT="$T20_RUN_DIR/s3-inner.log"

trap 't20_teardown' EXIT INT TERM
t20_bridge_up
t20_dind_up "$DIND" 10.240.0.10 s3
t20_stage "$DIND" "$INNER" /opt/in-lifecycle-latency.sh

start=$(date +%s)
docker exec "$DIND" sh /opt/in-lifecycle-latency.sh "$N" >"$OUT" 2>&1
rc=$?
end=$(date +%s)
t20_log "inner rc=$rc wall=$((end - start))s (log: $OUT)"

# 原始产物回取（exec+stdout 直出，不用 docker cp——宿主→dind 方向有丢文件已知问题）。
docker exec "$DIND" cat /tmp/t20-s3-events.log >"$T20_RUN_DIR/s3-events.log" 2>&1 || true
docker exec "$DIND" cat /tmp/t20-s3-metrics.txt >"$T20_RUN_DIR/s3-metrics.txt" 2>&1 || true

# 宿主侧分布统计（独立脚本，可对既有 run 目录复跑）。
sh "$ROOT/spike/t20/scripts/s3-stats.sh" "$T20_RUN_DIR"
t20_log "stats: $T20_RUN_DIR/s3-stats.txt"
