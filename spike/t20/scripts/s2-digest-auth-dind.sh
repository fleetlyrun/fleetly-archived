#!/bin/sh
# spike/t20/scripts/s2-digest-auth-dind.sh — Spike ② 跨节点 + 凭证真机腿。
#
# 双 dind swarm（29.8.1）+ 本地 registry:2（htpasswd Basic Auth）构造私有镜像：
#   A. 公共镜像「tag→digest 解析 → swarm 逐节点按 digest 拉取（零预拉）」
#      —— global 服务两任务；逐节点拉取证据 = dockerd "image pulled" 行。
#   B. EncodedRegistryAuth 真机语义：
#      B2 对照：不携 auth create → w1 拉取 401、任务 Rejected；
#      B1 携 auth create（CLI --with-registry-auth = API EncodedRegistryAuth
#         的 CLI 等价形态）→ w1（无本地登录）拉取成功；
#      B3 update 不携 auth 且换成新 tag（新 manifest digest）→ 新任务仍能拉取
#         = 服务 spec 的 PullOptions 凭据在 update 后留存（强制真拉，不靠缓存）；
#      B4 披露面：docker service inspect 原文不含凭据/PullOptions。
#
# 本机环境事实（影响证据形态，报告记录）：dind 为 containerd image store，
# swarm agent 拉取的镜像不注册进 docker image store（docker image ls 不可见）
# ——逐节点拉取以 dockerd 日志 + registry 访问日志为准。
#
# 用法（Git Bash，仓库根目录）：sh spike/t20/scripts/s2-digest-auth-dind.sh
set -u
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

SELF=$(printf '%s' "$0" | tr '\\' '/')
ROOT=$(CDPATH= cd -- "$(dirname -- "$SELF")/../../.." && pwd)
. "$ROOT/spike/t20/scripts/lib.sh"
cd "$ROOT" || t20_die 'cd repo root'

t20_need_docker
t20_init_run_dir s2-dind
MGR=t20-s2-mgr
W1=t20-s2-w1
REG=t20-s2-registry
REG_IP=10.240.0.50
REG_PORT=5000
REG_HOST="$REG_IP:$REG_PORT"
# 宿主（Docker Desktop/WSL2）不能直达容器 bridge IP（实测 000），registry
# 另发布回环端口供宿主侧解析探针/健康检查使用；dind 节点走 bridge IP。
REG_PUBLISH=127.0.0.1:15000
REG_USER=t20user
REG_PASS=t20pass
PRIVATE_IMAGE="$REG_HOST/t20/private:1"
PRIVATE_IMAGE_2="$REG_HOST/t20/private:2"
PUBLIC_IMAGE=alpine:3.19
PUSH_BASE=alpine:3.20
SUMMARY="$T20_RUN_DIR/s2-dind-summary.txt"
LOG="$T20_RUN_DIR/s2-dind.log"
AUTH_DIR="$T20_RUN_DIR/registry-auth"

say() { printf '%s\n' "$*" | tee -a "$SUMMARY"; }
m() { docker exec "$MGR" "$@"; }
w1() { docker exec "$W1" "$@"; }

cleanup() {
    docker rm -f "$REG" >/dev/null 2>&1 || true
    t20_teardown
}
trap cleanup EXIT INT TERM

snap_registry() { # <tag> — registry 访问日志快照（阶段证据）
    docker logs "$REG" >"$T20_RUN_DIR/s2-registry-$1.log" 2>&1 || true
    say "registry snapshot $1: $(grep -c . "$T20_RUN_DIR/s2-registry-$1.log") lines"
}

pull_lines() { # <dind> <grep-key> — dockerd 日志里的逐节点拉取证据
    docker logs "$1" 2>&1 | grep -E 'image pulled|remote=' | grep -F "$2" | tail -3
}

t20_bridge_up
t20_dind_up "$MGR" 10.240.0.10 mgr "--insecure-registry=$REG_HOST"
t20_dind_up "$W1" 10.240.0.11 w1 "--insecure-registry=$REG_HOST"

say "=== registry:2 (htpasswd) at $REG_HOST (host probe $REG_PUBLISH) ==="
mkdir -p "$AUTH_DIR" || t20_die "mkdir $AUTH_DIR"
if ! docker image inspect httpd:2.4-alpine >"$LOG" 2>&1; then
    docker pull -q httpd:2.4-alpine >>"$LOG" 2>&1 || t20_die 'pull httpd:2.4-alpine'
fi
docker run --rm httpd:2.4-alpine htpasswd -Bbn "$REG_USER" "$REG_PASS" >"$AUTH_DIR/htpasswd" 2>>"$LOG" ||
    t20_die 'htpasswd generation'
if command -v cygpath >/dev/null 2>&1; then
    AUTH_ABS=$(CDPATH= cd -- "$AUTH_DIR" && pwd) || t20_die "resolve $AUTH_DIR"
    AUTH_MNT=$(cygpath -w "$AUTH_ABS")
else
    AUTH_MNT=$AUTH_DIR
fi
docker run -d --name "$REG" --network "$T20_BR_NET" --ip "$REG_IP" \
    -p "$REG_PUBLISH:5000" \
    -v "$AUTH_MNT:/auth" \
    -e REGISTRY_AUTH=htpasswd \
    -e REGISTRY_AUTH_HTPASSWD_REALM=t20 \
    -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd \
    registry:2 >>"$LOG" 2>&1 || t20_die 'run registry'
i=0
while :; do
    code=$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' "http://$REG_PUBLISH/v2/" 2>&1 || true)
    [ "$code" = "401" ] && break
    i=$((i + 2))
    [ "$i" -ge 60 ] && t20_die "registry not answering 401 within 60s (code=$code)"
    sleep 2
done
say "registry up (anonymous /v2/ -> 401)"

say '=== swarm init + join ==='
m docker swarm init --advertise-addr eth0 >>"$LOG" 2>&1 || t20_die 'swarm init'
TOKEN=$(m docker swarm join-token -q worker) || t20_die 'join token'
w1 docker swarm join "10.240.0.10:2377" --token "$TOKEN" >>"$LOG" 2>&1 || t20_die 'w1 join'
i=0
while :; do
    ready=$(m sh -c "docker node ls --format '{{.Status}}' | grep -c '^Ready'")
    [ "$ready" -ge 2 ] && break
    i=$((i + 2))
    [ "$i" -ge 90 ] && t20_die 'two nodes not Ready'
    sleep 2
done
say 'two nodes Ready'

# ── A. 公共镜像零预拉：解析 + 逐节点拉取
say '=== A: tag->digest resolve + per-node digest pull (zero pre-pull) ==='
GO_OUT=$(cd "$ROOT" && go run ./spike/t20/cmd/t20resolve "$PUBLIC_IMAGE" 2>&1) || true
say "resolve: $GO_OUT"
PUBLIC_DIGEST=$(printf '%s\n' "$GO_OUT" | sed -n 's/^RESOLVED .* -> //p')
[ -n "$PUBLIC_DIGEST" ] || t20_die 'public digest resolution failed'
PUB_PINNED="$PUBLIC_IMAGE@$PUBLIC_DIGEST"

for d in "$MGR" "$W1"; do
    node_name=$([ "$d" = "$MGR" ] && printf 'mgr' || printf 'w1')
    pre=$(docker exec "$d" sh -c "docker image inspect '$PUB_PINNED' --format '{{.Id}}'" 2>&1 | tr -d '\r')
    say "A pre-check $node_name inspect($PUB_PINNED): ${pre:-<absent>}"
done

m docker service create --detach --name t20-pub --mode global --restart-condition none \
    --stop-grace-period 1s "$PUB_PINNED" sleep 600 >>"$LOG" 2>&1 || t20_die 'create t20-pub'
i=0
while :; do
    run_n=$(m sh -c "docker service ps t20-pub --format '{{.CurrentState}}' | grep -c '^Running'")
    [ "$run_n" -ge 2 ] && break
    i=$((i + 2))
    [ "$i" -ge 300 ] && break
    sleep 2
done
say "t20-pub running tasks=$run_n (want 2)"
m docker service ps t20-pub --format '{{.Name}} {{.Node}} {{.CurrentState}}' >>"$SUMMARY" 2>&1 || true
for d in "$MGR" "$W1"; do
    node_name=$([ "$d" = "$MGR" ] && printf 'mgr' || printf 'w1')
    pull_lines "$d" "$PUBLIC_DIGEST" | sed "s/^/A $node_name dockerd: /" >>"$SUMMARY"
    post=$(docker exec "$d" sh -c "docker image inspect '$PUB_PINNED' --format '{{.Id}}'" 2>&1 | tr -d '\r')
    say "A post-check $node_name inspect: ${post:-<absent (agent store)>}"
done
m docker service rm t20-pub >>"$LOG" 2>&1 || true
sleep 3

# ── B. 私有镜像：推送 + 凭证语义
say '=== B: private image push + EncodedRegistryAuth create/update semantics ==='
m docker pull -q "$PUSH_BASE" >>"$LOG" 2>&1 || t20_die 'mgr pull push-base'
printf '%s\n' "$REG_PASS" | docker exec -i "$MGR" docker login "$REG_HOST" -u "$REG_USER" --password-stdin \
    >>"$LOG" 2>&1 || t20_die 'mgr docker login'
m docker tag "$PUSH_BASE" "$PRIVATE_IMAGE" >>"$LOG" 2>&1 || t20_die 'tag private:1'
# tag 2 = 变体镜像（LABEL 改变 config → 新 manifest digest；层共享），用于
# B3「update 不携 auth 换新镜像」的强制真拉。
cat >"$T20_RUN_DIR/Dockerfile.private2" <<EOF
FROM $PUSH_BASE
LABEL t20.probe=2
EOF
t20_stage "$MGR" "$T20_RUN_DIR/Dockerfile.private2" /tmp/Dockerfile.private2
m docker build -q -t "$PRIVATE_IMAGE_2" -f /tmp/Dockerfile.private2 /tmp >>"$LOG" 2>&1 || t20_die 'build private:2'
m docker push "$PRIVATE_IMAGE" >>"$LOG" 2>&1 || t20_die 'push private:1'
m docker push "$PRIVATE_IMAGE_2" >>"$LOG" 2>&1 || t20_die 'push private:2'
PRIVATE_DIGEST_1=$(m docker inspect --format '{{index .RepoDigests 0}}' "$PRIVATE_IMAGE" | tr -d '\r')
PRIVATE_DIGEST_2=$(m docker inspect --format '{{index .RepoDigests 0}}' "$PRIVATE_IMAGE_2" | tr -d '\r')
say "pushed private:1 ($PRIVATE_DIGEST_1) and private:2 ($PRIVATE_DIGEST_2)"

# 平台解析腿：带凭证 tag→digest（Basic challenge flow）。宿主 Git Bash 带
# HTTP_PROXY 环境（本机环境事实），探针走回环发布端口（Go 默认绕过回环代理）。
RESOLVE_OUT=$(cd "$ROOT" && go run ./spike/t20/cmd/t20resolve \
    -user "$REG_USER" -pass "$REG_PASS" "$REG_PUBLISH/t20/private:1" 2>&1) || true
say "private resolve (host loopback): $RESOLVE_OUT"
AUTH_BLOB=$(cd "$ROOT" && go run ./spike/t20/cmd/t20resolve \
    -user "$REG_USER" -pass "$REG_PASS" -encode-auth-for "$REG_HOST" 2>&1 | sed -n 's/^AUTH_BLOB //p')
say "encoded auth blob computed (length ${#AUTH_BLOB})"

# B2 对照：不携 auth（tag 1）→ w1 拉取失败。
m docker service create --detach --name t20-noauth --constraint node.hostname==w1 \
    --restart-condition none --stop-grace-period 1s "$PRIVATE_IMAGE" sleep 60 >>"$LOG" 2>&1 ||
    t20_die 'create t20-noauth'
sleep 25
NOAUTH_PS=$(m sh -c "docker service ps t20-noauth --no-trunc --format '{{.CurrentState}} | {{.Error}}' | head -3")
say "B2 no-auth control task: $NOAUTH_PS"
snap_registry b2
m docker service rm t20-noauth >>"$LOG" 2>&1 || true
sleep 3

# B1 携 auth（CLI --with-registry-auth = API EncodedRegistryAuth 的 CLI 等价形态）。
m docker service create --detach --with-registry-auth --name t20-auth \
    --constraint node.hostname==w1 --restart-condition none --stop-grace-period 1s \
    "$PRIVATE_IMAGE" sleep 600 >>"$LOG" 2>&1 || t20_die 'create t20-auth'
i=0
while :; do
    auth_state=$(m sh -c "docker service ps t20-auth --format '{{.CurrentState}}' | head -1")
    case "$auth_state" in
    Running*) break ;;
    esac
    i=$((i + 2))
    [ "$i" -ge 180 ] && break
    sleep 2
done
say "B1 with-auth task: $auth_state"
pull_lines "$W1" "10.240.0.50:5000" | sed 's/^/B1 w1 dockerd: /' >>"$SUMMARY"
W1_CFG=$(w1 sh -c 'ls -l /root/.docker/config.json' 2>&1 | tr -d '\r')
say "B1 w1 docker login config: ${W1_CFG:-<absent>}"
snap_registry b1

# B4 披露面：inspect 原文零凭据/PullOptions（原文落库备查）。
m docker service inspect t20-auth >"$T20_RUN_DIR/s2-inspect-auth.json" 2>&1 || true
INSPECT_RAW=$(cat "$T20_RUN_DIR/s2-inspect-auth.json")
LEAK=0
printf '%s' "$INSPECT_RAW" | grep -qE 'RegistryAuth|PullOptions' && LEAK=1
printf '%s' "$INSPECT_RAW" | grep -qF "$REG_PASS" && LEAK=1
say "B4 service inspect leak check: $([ "$LEAK" -eq 0 ] && printf 'no-leak' || printf 'LEAK')"

# B3 update 不携 auth、换 tag 2（新 manifest digest）→ 新任务必须真拉新 manifest。
m docker service update --image "$PRIVATE_IMAGE_2" --detach t20-auth >>"$LOG" 2>&1 ||
    t20_die 'service update without auth'
i=0
while :; do
    upd_state=$(m sh -c "docker service ps t20-auth --format '{{.CurrentState}} | {{.Image}}' | head -1")
    case "$upd_state" in
    Running*"$PRIVATE_IMAGE_2"*) break ;;
    esac
    i=$((i + 2))
    [ "$i" -ge 180 ] && break
    sleep 2
done
say "B3 update-without-auth new task: $upd_state"
pull_lines "$W1" "10.240.0.50:5000" | sed 's/^/B3 w1 dockerd: /' >>"$SUMMARY"
snap_registry b3
B3_REQUESTS=$(grep -c '10.240.0.11' "$T20_RUN_DIR/s2-registry-b3.log" || true)
say "B3 w1 registry requests (cumulative): $B3_REQUESTS"
B1_REQUESTS=$(grep -c '10.240.0.11' "$T20_RUN_DIR/s2-registry-b1.log" || true)
say "B1 w1 registry requests (cumulative): $B1_REQUESTS (delta = $((B3_REQUESTS - B1_REQUESTS)))"

# 存储面取证：dind 内 swarm 数据目录形态 + 明文凭据检索。
{
    echo "--- swarm data dir listing (mgr) ---"
    m sh -c 'ls -la /var/lib/docker/swarm; echo; ls -la /var/lib/docker/swarm/raft'
    echo "--- plaintext credential grep (expect empty if raft encrypted) ---"
    m sh -c "grep -ral '$REG_USER' /var/lib/docker/swarm | head -3" || true
    echo "--- auth blob grep (exact X-Registry-Auth encoding) ---"
    if [ -n "$AUTH_BLOB" ]; then
        m sh -c "grep -ralF '$AUTH_BLOB' /var/lib/docker/swarm | head -3" || true
    fi
} >"$T20_RUN_DIR/s2-raft-probe.txt" 2>&1
say "raft probe: $T20_RUN_DIR/s2-raft-probe.txt ($(grep -c . "$T20_RUN_DIR/s2-raft-probe.txt") lines)"

# registry 侧访问日志汇总（成功/401 双面；阶段快照已单独落库）。
snap_registry final

m docker service rm t20-auth >>"$LOG" 2>&1 || true
sleep 3
say '=== S2-DIND DONE ==='
