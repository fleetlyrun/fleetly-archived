#!/bin/sh
# spike/t20/scripts/s2-resolve-digests.sh — Spike ② 解析腿（宿主，无需 dind）。
#
# 两条可复跑腿：
#   1) 生产解析客户端（internal/imageregistry）对真实 registry 的匿名
#      tag→digest 解析（Docker Hub + ghcr；含 Bearer token flow）；
#   2) 本地单节点零预拉 digest 部署复核（internal/substrate 手跑探针，
#      ghcr 公共镜像，清场本机镜像后 registry-first 解析 + digest 钉定建服务）。
# 用法（Git Bash，仓库根目录）：sh spike/t20/scripts/s2-resolve-digests.sh
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

SELF=$(printf '%s' "$0" | tr '\\' '/')
ROOT=$(CDPATH= cd -- "$(dirname -- "$SELF")/../../.." && pwd)
. "$ROOT/spike/t20/scripts/lib.sh"
cd "$ROOT" || t20_die 'cd repo root'

t20_need_docker
t20_init_run_dir s2-resolve
RESOLVE_LOG="$T20_RUN_DIR/s2-resolve-registry.log"
ZEROPULL_LOG="$T20_RUN_DIR/s2-resolve-zero-pull.log"
RESOLVER_PROBE="$ROOT/spike/t20/cmd/t20resolve"

t20_log 'leg 1: production resolver against real registries (anonymous)'
if FLEETLY_MANUAL_REGISTRY=1 go test -count=1 -tags manual ./internal/imageregistry \
    -run TestManualResolvePublicRegistryImages -v >"$RESOLVE_LOG" 2>&1; then
    t20_log 'leg 1 rc=0'
else
    t20_log "leg 1 rc=$? (see $RESOLVE_LOG)"
fi
grep -E 'RESOLVED|->|--- (PASS|FAIL)' "$RESOLVE_LOG" | tee "$T20_RUN_DIR/s2-resolve-summary.txt"

t20_log 'leg 1b: same resolver via spike probe (explicit output lines)'
{
    go run ./spike/t20/cmd/t20resolve alpine:3.19
    go run ./spike/t20/cmd/t20resolve ghcr.io/linuxserver/sonarr:latest
} >>"$T20_RUN_DIR/s2-resolve-summary.txt" 2>&1 || t20_log 'leg 1b saw a failure (see summary)'

t20_log 'leg 2: zero-pre-pull digest deploy on the local single-node swarm'
if FLEETLY_MANUAL_SWARM=1 go test -count=1 -tags manual ./internal/substrate \
    -run TestManualPublicImageZeroPrePull -v >"$ZEROPULL_LOG" 2>&1; then
    t20_log 'leg 2 rc=0'
else
    t20_log "leg 2 rc=$? (see $ZEROPULL_LOG)"
fi
grep -E 'resolved |task |zero-pre-pull|PASS|FAIL' "$ZEROPULL_LOG" | tail -20

t20_log "artifacts: $T20_RUN_DIR"
