#!/bin/sh
# spike/t20/scripts/s3-stats.sh <run-dir> — Spike ③ 时延分布统计（可独立复跑）。
#
# 读 <run-dir>/s3-metrics.txt（in-lifecycle-latency.sh 产物），输出
# <run-dir>/s3-stats.txt 并回显：每指标的 n/min/p50/p90/max（ms）。
set -u

RUN_DIR=${1:?usage: s3-stats.sh <run-dir>}
METRICS="$RUN_DIR/s3-metrics.txt"
STATS="$RUN_DIR/s3-stats.txt"
[ -f "$METRICS" ] || {
    echo "missing $METRICS" >&2
    exit 1
}

{
    echo "# Spike ③ 生命周期时延分布（ms；min/p50/p90/max；来源 s3-metrics.txt）"
    for metric in "A create_to_running_ms" "A create_to_healthy_ms" "A update_to_running_ms" "A update_to_healthy_ms" "A rm_to_destroy_ms" "A rm_to_die_ms" "B create_to_running_ms" "B create_to_healthy_ms" "B start_to_die_ms"; do
        vals=$(grep -F -e "$metric=" "$METRICS" | sed 's/^[^=]*=//' | sed 's/ .*//' | sort -n)
        n=$(printf '%s\n' "$vals" | grep -c . || true)
        [ "$n" -gt 0 ] || continue
        p50=$(printf '%s\n' "$vals" | awk -v n="$n" 'NR==int((n+1)/2)')
        p90=$(printf '%s\n' "$vals" | awk -v n="$n" 'NR==int(n*0.9) || (n==1 && NR==1)')
        min=$(printf '%s\n' "$vals" | head -1)
        max=$(printf '%s\n' "$vals" | tail -1)
        echo "$metric n=$n min=$min p50=$p50 p90=$p90 max=$max"
    done
} >"$STATS"
cat "$STATS"
