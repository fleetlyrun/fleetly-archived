#!/bin/sh
# deploy/run-journey-test.sh — T2.26 v0.1 端到端验收旅程的宿主编排（架构
# §4.2 验收段的 dind 全链版：一条命令安装 → API/webhook 部署拿到 HTTPS 200
# （Pebble 链路，T2-6 实证模式的回归化；git push(SSH) 收包面 2026-09-29
# 移除，ADR-0012——J3 CLI deploy + J6 签名 webhook 投递+拉源双通道）→ UI
# 数据可见 → 一键回滚 → 全程计时 ≤20min 硬断言。真 VPS dogfooding 属 M2
# 后置项（delivery-pipeline §2.6，口径见 docs/reports/2026-09-19-v0.1-acceptance.md）。模式照抄
# run-upgrade-test.sh：交叉编译 fleetlyd+fleetly+探针 → 宿主构建 probeapp:1
# 镜像（docker save|gzip）→ 起 docker:29.8.1-dind 特权容器 → exec+stdin
# 注入（禁用 docker cp——Engine 29.x 宿主→特权 dind 会 exit 0 但文件不落
# 盘）→ 容器内跑 test-journey.sh → 产物（summary.md/journey.json）经
# docker exec cat 读回 → 清理。可在 GitHub Actions（ubuntu-latest）与本地
# Git Bash（Windows）/ Linux shell 复跑。
#
# usage: run-journey-test.sh
# env:
#   DIND_IMAGE      dind 镜像（默认 docker:29.8.1-dind，与 CI/引擎门禁一致）
#   DIND_NAME       容器名（默认 fleetly-journey-test）
#   JK_SKIP_BUILD   1 = 跳过交叉编译，改用 JK_BIN_DIR 指定的二进制目录
#   JK_BIN_DIR      JK_SKIP_BUILD=1 时的二进制目录（需含 fleetlyd 与 fleetly）
#   JK_VERSION      注入的版本串（默认 v0.1.0-journey）
#   JK_MEMORY       dind 容器内存上限（默认 4g；0 = 不设限）
#   JK_OUT_DIR      宿主侧产物目录（默认 $TMP/journey）
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

# dind 镜像钉 digest（T0-V2.3 供应链）：默认值 tag@sha256——tag 保留作可读性，
# digest 为准；env 覆盖仍可用（显式传入即按传入值起容器）。
DIND_IMAGE="${DIND_IMAGE:-docker:29.8.1-dind@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0}"
DIND_NAME="${DIND_NAME:-fleetly-journey-test}"
JK_SKIP_BUILD="${JK_SKIP_BUILD:-0}"
JK_VERSION="${JK_VERSION:-v0.1.0-journey}"
JK_MEMORY="${JK_MEMORY:-4g}"

log() { printf '[journey %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() {
    log "FATAL: $*"
    exit 1
}
cleanup() {
    docker rm -f "$DIND_NAME" >/dev/null 2>&1 || true
}
trap 'rm -rf "$TMP"; cleanup' EXIT

posix_path() {
    printf '%s' "$1" | tr '\\' '/'
}

SELF=$(posix_path "$0")
ROOT=$(CDPATH= cd -- "$(dirname -- "$SELF")/.." && pwd)
TMP=$(posix_path "$(mktemp -d)")
case "$TMP" in
/*)
    if command -v cygpath >/dev/null 2>&1; then
        TMP=$(cygpath -m "$TMP") || die 'cygpath -m on tmpdir'
    fi
    ;;
esac

command -v docker >/dev/null 2>&1 || die 'docker not on PATH'
command -v go >/dev/null 2>&1 || JK_SKIP_BUILD=1

stage() { # <remote-path> <local-file> — exec+stdin 直传 + sha256 双校验 + CR 剥离
    _r=$1
    _f=$2
    docker exec -i "$DIND_NAME" sh -c "cat > '$_r'" <"$_f" ||
        die "staging $_r"
    _h=$(sha256sum <"$_f" 2>/dev/null)
    _h=${_h%% *}
    _g=$(docker exec "$DIND_NAME" sh -c "sha256sum '$_r'" 2>/dev/null)
    _g=${_g%% *}
    if [ -z "$_h" ] || [ "$_h" != "$_g" ]; then
        die "sha256 mismatch/absent for $_r: host=$_h dind=$_g"
    fi
    case "$_r" in
    *.sh | *.yaml)
        docker exec "$DIND_NAME" sed -i 's/\r$//' "$_r" ||
            die "strip CR from $_r"
        ;;
    esac
}

# ------------------------------------------------------------------ 构建
log "repo root: $ROOT  tmp: $TMP"
if [ "$JK_SKIP_BUILD" != '1' ]; then
    log "cross-compiling linux/amd64 fleetlyd+fleetly+probe (version $JK_VERSION)"
    (
        cd "$ROOT" &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$JK_VERSION" \
                -o "$TMP/fleetlyd" ./cmd/fleetlyd &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w -X main.version=$JK_VERSION" \
                -o "$TMP/fleetly" ./cmd/fleetly &&
            GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -trimpath -ldflags "-s -w" \
                -o "$TMP/hello" ./deploy/testdata/probeapp
    ) || die 'go build failed'
    JK_BIN_DIR="$TMP"
else
    JK_BIN_DIR="${JK_BIN_DIR:?JK_SKIP_BUILD=1 requires JK_BIN_DIR}"
    log "using prebuilt binaries from $JK_BIN_DIR"
fi
for _f in "$JK_BIN_DIR/fleetlyd" "$JK_BIN_DIR/fleetly" "$JK_BIN_DIR/hello"; do
    [ -f "$_f" ] || die "missing: $_f"
done
for _f in deploy/install.sh deploy/test-journey.sh; do
    [ -f "$ROOT/$_f" ] || die "missing: $ROOT/$_f"
done

# 宿主侧构建探针镜像（scratch + 静态二进制，零 registry 依赖）并导出。
printf 'FROM scratch\nCOPY hello /probe\nENTRYPOINT ["/probe"]\n' >"$TMP/probeimg.Dockerfile"
mkdir -p "$TMP/probeimg"
cp "$JK_BIN_DIR/hello" "$TMP/probeimg/hello" || die 'cp hello'
docker build --platform linux/amd64 -t probeapp:1 -f "$TMP/probeimg.Dockerfile" "$TMP/probeimg" ||
    die 'host docker build of probe image failed'
docker save probeapp:1 | gzip >"$TMP/probe.tar.gz" ||
    die 'docker save of probe image failed'
log "probe image tarball: $(ls -la "$TMP/probe.tar.gz" 2>/dev/null | tr -s ' ')"

# Console dist 现成产物清点（注入而非 dind 内构建——T2.26 口径：注入更稳）。
DIST="$ROOT/console/dist"
[ -f "$DIST/index.html" ] || die "console/dist/index.html missing (run pnpm build in console/ first)"
DIST_FILES=$(cd "$DIST" && find . -type f | sed 's|^\./||')
[ -n "$DIST_FILES" ] || die 'console/dist has no files'
log "console dist files: $(printf '%s' "$DIST_FILES" | wc -l | tr -d ' ')"

# ----------------------------------------------------------------- dind
docker rm -f "$DIND_NAME" >/dev/null 2>&1 || true
_MEM_ARGS=''
case "$JK_MEMORY" in
0 | none | '') log 'dind memory cap: none (JK_MEMORY=0)' ;;
*)
    _MEM_ARGS="--memory $JK_MEMORY --memory-swap $JK_MEMORY"
    log "dind memory cap: $JK_MEMORY"
    ;;
esac
# shellcheck disable=SC2086
docker run -d --name "$DIND_NAME" --privileged $_MEM_ARGS "$DIND_IMAGE" >/dev/null ||
    die "docker run $DIND_NAME"
_i=0
while ! docker exec "$DIND_NAME" docker info >/dev/null 2>&1; do
    _i=$((_i + 2))
    if [ "$_i" -ge 60 ]; then
        docker logs "$DIND_NAME" --tail 40 || true
        docker rm -f "$DIND_NAME" >/dev/null 2>&1 || true
        die 'inner dockerd not ready within 60s'
    fi
    sleep 2
done
log "dind $DIND_NAME ready"

# ----------------------------------------------------------------- 注入
log 'staging scripts + binaries + console dist via exec+stdin'
docker exec "$DIND_NAME" mkdir -p /tmp/journey/bin /tmp/journey/console-dist || die 'mkdir stage'
stage /tmp/journey/install.sh "$ROOT/deploy/install.sh"
stage /tmp/journey/test-journey.sh "$ROOT/deploy/test-journey.sh"
stage /tmp/journey/fleetlyd "$JK_BIN_DIR/fleetlyd"
stage /tmp/journey/fleetly "$JK_BIN_DIR/fleetly"
stage /tmp/journey/hello "$JK_BIN_DIR/hello"
stage /tmp/journey/probe.tar.gz "$TMP/probe.tar.gz"
docker exec "$DIND_NAME" chmod +x /tmp/journey/fleetlyd /tmp/journey/fleetly /tmp/journey/hello ||
    die 'chmod binaries'
# console dist 逐文件注入（exec+stdin 同一通道；二进制资源不受 CR 剥离影响）。
printf '%s\n' "$DIST_FILES" | while IFS= read -r _rel; do
    [ -n "$_rel" ] || continue
    _dir=$(dirname "/tmp/journey/console-dist/$_rel")
    docker exec "$DIND_NAME" mkdir -p "$_dir" || die "mkdir $_dir"
    stage "/tmp/journey/console-dist/$_rel" "$DIST/$_rel"
done

# ----------------------------------------------------------------- 执行
log 'running in-dind journey suite (deploy/test-journey.sh)'
docker exec "$DIND_NAME" sh /tmp/journey/test-journey.sh
RC=$?

# ------------------------------------------------------- 产物读回（宿主）
JK_OUT_DIR="${JK_OUT_DIR:-$TMP/journey}"
if [ "$RC" -eq 0 ]; then
    mkdir -p "$JK_OUT_DIR" || die "mkdir $JK_OUT_DIR"
    docker exec "$DIND_NAME" cat /tmp/journey-out/summary.md >"$JK_OUT_DIR/summary.md" ||
        die 'read back summary.md'
    docker exec "$DIND_NAME" cat /tmp/journey-out/journey.json >"$JK_OUT_DIR/journey.json" ||
        die 'read back journey.json'
    log "artifacts: $JK_OUT_DIR/summary.md $JK_OUT_DIR/journey.json"
fi
if [ "$RC" -ne 0 ]; then
    log "suite RED (rc=$RC) — dumping dind log tail"
    docker logs "$DIND_NAME" --tail 120 2>&1 | tail -60 || true
fi
cleanup
if [ "$RC" -eq 0 ]; then
    log 'JOURNEY-TEST GREEN'
else
    log "JOURNEY-TEST RED (rc=$RC)"
fi
exit "$RC"
