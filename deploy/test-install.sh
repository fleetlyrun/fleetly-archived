#!/bin/sh
# deploy/test-install.sh — T2.1 安装器 dind 验收（在 docker:29.8.1-dind 容器内
# 执行；宿主侧编排命令见 deploy/README.md）。
#
# 前置：宿主已把以下文件经 exec+stdin 注入 $TI_STAGE（默认 /tmp/install-test，
# docker cp 在 Engine 29.x 宿主→特权 dind 会静默丢文件，禁用）：
#   $TI_STAGE/install.sh          deploy/install.sh
#   $TI_STAGE/uninstall.sh        deploy/uninstall.sh
#   $TI_STAGE/upgrade.sh          deploy/upgrade.sh（A11 公钥一致性断言用）
#   $TI_STAGE/fleetlyd.service    deploy/fleetlyd.service（unit 参考模板）
#   $TI_STAGE/bin/fleetlyd        linux 二进制（交叉编译，CGO_ENABLED=0）
#   $TI_STAGE/bin/fleetly         linux 二进制
#   $TI_STAGE/testdata/release-test-key.pem
#                                 openssl 轨测试签名私钥（A11 staged release
#                                 签名用；deploy/testdata，仅测试——生产密钥
#                                 另持，见 deploy/README.md release 签名密钥小节）
#
# 断言清单（对应 stage 验收标准）：
#   A1  语法：install.sh / uninstall.sh / fleetlyd.service 三件套 sh -n 通过
#   A2  正路径：--bin-dir + --no-systemd 安装成功（目录/二进制/配置/参考
#       unit/符号链接齐全；unit 与 deploy/fleetlyd.service 模板逐字一致；
#       报告含 advertise-addr 判定与端口面提示）
#   A3  swarm init：dind 内无 swarm → 安装后 Swarm active，advertise-addr
#       为私网 IP（dind 环境判定）
#   A4  daemon 手动启动（--no-systemd 口径）→ /healthz/liveness 200；
#       gRPC 8421 只绑回环（git SSH 8424 面已移除，ADR-0012）
#   A5  CLI 可连：bootstrap token 从 <数据根>/bootstrap-token 文件读取
#       （B5：token 不进日志）→ fleetly apps list
#   A6  重装幂等：已有 swarm → 跳过 init 不报错；已有 config 保留
#   A7  门禁负路径（版本）：假 docker（28.3.2）→ 拒绝、退出非零、输出原因
#   A8  门禁负路径（iptables）：nftables-only shim → 拒绝、退出非零
#   A9  卸载：unit/二进制/配置/符号链接消失，/var/lib/fleetly 保留且明示；
#       --purge 后数据目录消失
#   A10 探测 host 推导（整改⑤）：health_probe_host 对非回环 bind 地址打
#       真实 host、通配/空 host 回落 127.0.0.1（非回环绑定时硬编码回环
#       探测恒拒连——安装报红但平台在跑的假死形态）
#   A11 openssl 轨双轨验签（S14/H15）：staged release（测试私钥签出
#       .sig.pem）+ 假 curl（github download URL → 本地目录）端到端跑
#       install.sh 的 release 下载形态——① 正例：无 cosign（dind 常态）
#       openssl 轨验签通过 + 安装成功；② 篡改 checksums（内部自一致）→
#       openssl 验签 die；③ 双签名产物均缺 → 历史版本兼容警示路径通过；
#       ④ 仅 bundle（.sig.pem 缺）且无 cosign → die（降级即死）；⑤
#       --skip-signature-verify 显式跳过；另断言 install.sh/upgrade.sh
#       内嵌公钥与 testdata 测试公钥三方一致（防漂移/防脱钩）
#   A12 缺省安装 config 逐字节等价 v0.1（E1-1 硬断言）：金样 diff + 多节点
#       新键（base_domain/config_tls_addr/token_rotate/registry.*/auth_file）
#       零出现
#   A13 --base-domain 全链（E1-1 正路径，A9 purge 后）：config 含
#       base_domain 键；报告含 ctrl/registry/console 子域行 + DNS verify
#       提示 + 8423 端口行；安装器最小集纪律（不写 registry/join 键）；
#       已有 config + --base-domain → 保留 + 显式警示；非法域名拒绝并
#       回显原因
#
# 断言风格与 e2e/nightly/lib.sh 一致（NAME: PASS/FAIL + 退出码），但独立
# 存放（e2e/ 只读，本脚本不引用它）。

set -u
# shellcheck disable=SC2034
NL_FAIL=0

TI_STAGE="${TI_STAGE:-/tmp/install-test}"
STAGE_BIN="$TI_STAGE/bin"
INSTALL_SH="$TI_STAGE/install.sh"
UNINSTALL_SH="$TI_STAGE/uninstall.sh"
UPGRADE_SH="$TI_STAGE/upgrade.sh"
TEST_SIGN_KEY="$TI_STAGE/testdata/release-test-key.pem"
SERVICE_TPL="$TI_STAGE/fleetlyd.service"
INSTALL_LOG="/tmp/ti-install.log"
REINSTALL_LOG="/tmp/ti-reinstall.log"
NEG_VER_LOG="/tmp/ti-neg-version.log"
NEG_IPT_LOG="/tmp/ti-neg-iptables.log"
SIG_OK_LOG="/tmp/ti-sig-ok.log"
SIG_TAMPER_LOG="/tmp/ti-sig-tamper.log"
SIG_NOSIG_LOG="/tmp/ti-sig-nosig.log"
SIG_NOPEM_LOG="/tmp/ti-sig-nopem.log"
SIG_SKIP_LOG="/tmp/ti-sig-skip.log"
UNINSTALL_LOG="/tmp/ti-uninstall.log"
DLOG="/tmp/ti-fleetlyd.log"

nl() { printf '[ti %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
pass() { nl "$1: PASS"; }
fail() {
    nl "$1: FAIL ${2:-}"
    # shellcheck disable=SC2034
    NL_FAIL=$((NL_FAIL + 1))
}
assert() { # <name> <0|1> [detail]
    if [ "$2" -eq 0 ]; then
        pass "$1"
    else
        fail "$1" "${3:-}"
    fi
}
finish() {
    if [ "$NL_FAIL" -eq 0 ]; then
        nl "SUITE-DONE all-asserts-passed"
        exit 0
    fi
    nl "SUITE-DONE failed-asserts=$NL_FAIL"
    nl "---- fleetlyd log tail ($DLOG) ----"
    tail -n 40 "$DLOG" 2>/dev/null || true
    nl "---- end fleetlyd log tail ----"
    exit 1
}

have() { command -v "$1" >/dev/null 2>&1; }

http_get() { # <url> — 2xx 时退出 0
    if have curl; then
        curl -fsS --max-time 3 -o /dev/null "$1"
    else
        wget -q -T 3 -O /dev/null "$1" 2>/dev/null
    fi
}

nl "=== T2.1 installer dind suite (stage=$TI_STAGE) ==="

# ---------------------------------------------------- preflight + A1 语法
for _f in "$INSTALL_SH" "$UNINSTALL_SH" "$UPGRADE_SH" "$SERVICE_TPL" \
    "$TEST_SIGN_KEY" "$STAGE_BIN/fleetlyd" "$STAGE_BIN/fleetly"; do
    [ -s "$_f" ] || {
        nl "FATAL: staged file missing: $_f (host-side exec+stdin staging incomplete)"
        exit 1
    }
done
# 本地检出可能是 CRLF：busybox ash 无法执行带 CR 的脚本——统一就地剥离
# （测试签名私钥 PEM 同理：openssl 的 PEM 解析不吃 CRLF）。
sed -i 's/\r$//' "$INSTALL_SH" "$UNINSTALL_SH" "$UPGRADE_SH" "$SERVICE_TPL" "$TEST_SIGN_KEY" 2>/dev/null || true

sh -n "$INSTALL_SH"
assert "A1-shn-install" $?
sh -n "$UNINSTALL_SH"
assert "A1-shn-uninstall" $?

# ---------------------------------------------------------- A2 正路径安装
# 门禁要求 dind 引擎 >= 29.8.1（镜像 docker:29.8.1-dind 保证）。
ENG=$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo unknown)
nl "inner engine: $ENG"

sh "$INSTALL_SH" --bin-dir "$STAGE_BIN" --no-systemd >"$INSTALL_LOG" 2>&1
RC=$?
assert "A2-install-rc0" "$RC" "rc=$RC"
if [ "$RC" -ne 0 ]; then
    nl "---- install output ----"
    cat "$INSTALL_LOG" || true
    finish
fi

[ -x /opt/fleetly/bin/fleetlyd ]
assert "A2-fleetlyd-installed" $?
[ -x /opt/fleetly/bin/fleetly ]
assert "A2-fleetly-installed" $?
[ -f /opt/fleetly/etc/config.yaml ]
assert "A2-config-written" $?
[ -x /usr/local/bin/fleetly ]
assert "A2-cli-symlink" $?
[ "$(ls -l /usr/local/bin/fleetly 2>/dev/null | sed -n 's/.*-> //p')" = "/opt/fleetly/bin/fleetly" ]
assert "A2-cli-symlink-target" $?

grep -q 'addr: "0.0.0.0:8420"' /opt/fleetly/etc/config.yaml
assert "A2-config-http-addr" $?
grep -q '127.0.0.1:8421' /opt/fleetly/etc/config.yaml
assert "A2-config-grpc-loopback" $?
grep -q '/var/lib/fleetly/fleetly.db' /opt/fleetly/etc/config.yaml
assert "A2-config-db-under-data-root" $?
# git push(SSH) 面已移除（ADR-0012）——生成配置不再含 8424 监听键。
! grep -q '8424' /opt/fleetly/etc/config.yaml
assert "A2-config-git-ssh-absent" $?

# unit 参考副本与 deploy/fleetlyd.service 模板逐字一致（防两份漂移）。
diff /opt/fleetly/etc/fleetlyd.service "$SERVICE_TPL" >/dev/null 2>&1
assert "A2-unit-matches-template" $?
grep -q 'After=.*docker\.service' /opt/fleetly/etc/fleetlyd.service
assert "A2-unit-after-docker" $?
grep -q 'Restart=on-failure' /opt/fleetly/etc/fleetlyd.service
assert "A2-unit-restart" $?
grep -q 'WorkingDirectory=/var/lib/fleetly' /opt/fleetly/etc/fleetlyd.service
assert "A2-unit-workingdir" $?

# 报告：版本/引擎/后端/advertise-addr 判定/端口面/token 指引。
grep -q 'engine gate' "$INSTALL_LOG"
assert "A2-report-engine-gate-line" $?
grep -q 'iptables backend: pass' "$INSTALL_LOG"
assert "A2-report-iptables-pass" $?
grep -q 'advertise-addr' "$INSTALL_LOG"
assert "A2-report-advertise-addr" $?
grep -q 'port exposure' "$INSTALL_LOG"
assert "A2-report-port-exposure-section" $?
! grep -q '8424' "$INSTALL_LOG"
assert "A2-report-git-port-absent" $?
grep -q '2377' "$INSTALL_LOG"
assert "A2-report-swarm-ports" $?
grep -q 'bootstrap token' "$INSTALL_LOG"
assert "A2-report-token-hint" $?

# -------------------------------------------------------------- A3 swarm
[ "$(docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null)" = 'active' ]
assert "A3-swarm-active" $?
ADV=$(docker info --format '{{.Swarm.NodeAddr}}' 2>/dev/null | sed 's/:2377$//')
[ -n "$ADV" ]
assert "A3-advertise-addr-set" $?
case "$ADV" in
10.* | 192.168.* | 172.1[6-9].* | 172.2* | 172.3[01].*) assert "A3-advertise-private" 0 ;;
*) assert "A3-advertise-private" 1 "adv=$ADV" ;;
esac

# ------------------------------------- A4 daemon 手动启动 + 端口面 + liveness
(
    cd /var/lib/fleetly && exec /opt/fleetly/bin/fleetlyd -c /opt/fleetly/etc/config.yaml
) >"$DLOG" 2>&1 &
DPID=$!

_DEADLINE=$(( $(date +%s) + 90 ))
_LIVE=0
while [ "$(date +%s)" -lt "$_DEADLINE" ]; do
    if ! kill -0 "$DPID" 2>/dev/null; then
        break
    fi
    if http_get "http://127.0.0.1:8420/healthz/liveness"; then
        _LIVE=1
        break
    fi
    sleep 2
done
# assert 的参数约定是 rc 语义（0 = 通过）；_LIVE 是布尔语义（1 = 就绪），
# 换算后再断言——直接传会把 PASS 判成 FAIL。
if [ "$_LIVE" -eq 1 ]; then
    assert "A4-liveness-200" 0
else
    assert "A4-liveness-200" 1 "state=$_LIVE (deadline exceeded or process exited)"
fi
if [ "$_LIVE" -eq 0 ]; then
    nl "---- fleetlyd log tail ----"
    tail -n 40 "$DLOG" || true
fi

# git SSH 8424 面已移除（ADR-0012）——端口不应监听。
! netstat -tln 2>/dev/null | grep -q ':8424 '
assert "A4-git-ssh-not-listening" $?
netstat -tln 2>/dev/null | grep -q '127.0.0.1:8421 '
assert "A4-grpc-loopback-listening" $?
# HTTP 面按配置绑 0.0.0.0:8420，Go 通配监听可能落 [::]:8420（双栈）——
# busybox netstat 把 IPv6 通配渲染成 :::8420，三种形态都认。
netstat -tln 2>/dev/null | grep -E '(:::|0\.0\.0\.0:|\[::\]:)8420 ' >/dev/null
assert "A4-http-public-listening" $?

# --------------------------------------------------------------- A5 CLI
# B5：token 本体不再进日志——首启写入 <数据根>/bootstrap-token（0600）；
# 这里直接读文件（cat 剥掉行尾换行）。
TOK=$(cat /var/lib/fleetly/bootstrap-token 2>/dev/null)
[ -n "$TOK" ]
assert "A5-bootstrap-token-file" $?
if [ -n "$TOK" ]; then
    FLEETLY_ADDR=127.0.0.1:8421 FLEETLY_TOKEN="$TOK" /opt/fleetly/bin/fleetly apps list >"/tmp/ti-cli.out" 2>&1
    assert "A5-cli-apps-list" $? "$(cat /tmp/ti-cli.out 2>/dev/null | tail -3)"
else
    fail "A5-cli-apps-list" "no token in log"
fi

# 优雅停服（e2e/smoke 契约：SIGTERM → 退出码 0）。
kill -TERM "$DPID" 2>/dev/null || true
wait "$DPID" 2>/dev/null
_STOP_RC=$?
assert "A4-daemon-sigterm-rc0" "$_STOP_RC" "rc=$_STOP_RC"

# ------------------------------------------------------- A6 重装（幂等）
CFG_SHA_BEFORE=$(sha256sum /opt/fleetly/etc/config.yaml | awk '{print $1}')
sh "$INSTALL_SH" --bin-dir "$STAGE_BIN" --no-systemd >"$REINSTALL_LOG" 2>&1
RC=$?
assert "A6-reinstall-rc0" "$RC" "rc=$RC"
grep -q 'already active' "$REINSTALL_LOG"
assert "A6-swarm-init-skipped" $?
CFG_SHA_AFTER=$(sha256sum /opt/fleetly/etc/config.yaml | awk '{print $1}')
[ "$CFG_SHA_BEFORE" = "$CFG_SHA_AFTER" ]
assert "A6-config-preserved" $?

# --------------------- A12 缺省安装 config 逐字节等价 v0.1（E1-1 硬断言）
# install.sh 缺省（不填 --base-domain）的生成配置必须与 v0.1 逐字节一致——
# E1-1 只允许「显式给出 --base-domain 时追加键」。金样 = install.sh 缺省
# 配置生成段的快照（A2 形态：--http-addr 缺省 0.0.0.0:8420 + 数据根
# /var/lib/fleetly）；改 install.sh 缺省输出必须同步本金样并在说明里记录
# （这正是本断言要拦的静默漂移）。另负向断言多节点新键零出现。
cat > /tmp/ti-golden-config.yaml <<'GOLDEN_EOF'
# fleetlyd config — generated by deploy/install.sh (T2.1)
# 全量键位与注释口径见 config-example.yaml；此处只写显式落定的最小集，
# 其余键回落 internal/* 各 Config.Normalize 的平台默认。

# HTTP 面（REST/gateway + healthz + Console 托管）：绑定 0.0.0.0 对外提供
# 面板/API（公网部署请配合防火墙或反代，见安装报告端口暴露面）。
addr: "0.0.0.0:8420"

# gRPC 面（CLI/SDK 直连）：只绑回环——内部服务默认不暴露公网。
grpc:
  addr: "127.0.0.1:8421"

# 状态层与数据根。
state:
  db_path: "/var/lib/fleetly/fleetly.db"

secrets:
  key_path: "/var/lib/fleetly/fleetly.key"

build:
  cache_dir: "/var/lib/fleetly/build-cache"
  artifacts_dir: "/var/lib/fleetly/build-artifacts"

logs:
  dir: "/var/lib/fleetly/fleetly-logs"

ingress:
  token_file: "/var/lib/fleetly/fleetly-ingress.token"
  cert_dir: "/var/lib/fleetly/fleetly-certs"

# git webhook 触发入口：bare 仓库根（webhook 拉源的 fetch 落点）。git push
# (SSH) 收包面已移除（ADR-0012），无监听端口。
git:
  root: "/var/lib/fleetly/git"

logging:
  level: info
GOLDEN_EOF
diff /tmp/ti-golden-config.yaml /opt/fleetly/etc/config.yaml >/dev/null 2>&1
assert "A12-default-config-byte-equal-v0.1" $?
grep -q -e 'base_domain' -e 'config_tls_addr' -e 'token_rotate' -e '^registry:' -e 'auth_file' /opt/fleetly/etc/config.yaml
[ $? -ne 0 ]
assert "A12-default-config-has-no-multinode-keys" $?

# --------------------------------------------- A7 门禁负路径（引擎版本）
FAKE1="/tmp/ti-fakebin-ver"
mkdir -p "$FAKE1"
cat >"$FAKE1/docker" <<EOF
#!/bin/sh
case " \$* " in
*"{{.Server.Version}}"*) echo "28.3.2"; exit 0 ;;
esac
exit 0
EOF
chmod +x "$FAKE1/docker"
PATH="$FAKE1:$PATH" sh "$INSTALL_SH" --bin-dir "$STAGE_BIN" --no-systemd >"$NEG_VER_LOG" 2>&1
RC=$?
[ "$RC" -ne 0 ]
assert "A7-low-engine-rejected" $? "rc=$RC (want non-zero)"
grep -q "28.3.2" "$NEG_VER_LOG"
assert "A7-low-engine-reason-echoed" $?
grep -q "29.8.1" "$NEG_VER_LOG"
assert "A7-low-engine-floor-echoed" $?

# -------------------------------------------- A8 门禁负路径（iptables）
FAKE2="/tmp/ti-fakebin-ipt"
mkdir -p "$FAKE2"
REAL_DOCKER=$(command -v docker)
cat >"$FAKE2/docker" <<EOF
#!/bin/sh
exec $REAL_DOCKER "\$@"
EOF
cat >"$FAKE2/iptables" <<'EOF'
#!/bin/sh
echo "iptables v1.8.13 (nf_tables)"
exit 0
EOF
cat >"$FAKE2/iptables-legacy" <<'EOF'
#!/bin/sh
echo "iptables v1.8.13 (nf_tables)"
exit 0
EOF
chmod +x "$FAKE2/docker" "$FAKE2/iptables" "$FAKE2/iptables-legacy"
PATH="$FAKE2:$PATH" sh "$INSTALL_SH" --bin-dir "$STAGE_BIN" --no-systemd >"$NEG_IPT_LOG" 2>&1
RC=$?
[ "$RC" -ne 0 ]
assert "A8-nftables-rejected" $? "rc=$RC (want non-zero)"
grep -qi 'iptables' "$NEG_IPT_LOG"
assert "A8-nftables-reason-echoed" $?

# ------------------------------------- A10 探测 host 推导（整改⑤）
# install.sh 的健康探测必须打在 daemon 真实绑定的地址上：--http-addr 绑
# 非回环地址（如 10.0.0.5:8420）时 daemon 只绑该地址，127.0.0.1 探测恒
# 拒连——60s 假死后 die，而 systemctl enable --now 已成功（安装报红但
# 平台在跑）。最小可靠形态：把 health_probe_host 定义从 install.sh 原样
# 抽出直接喂样本断言（dind 内不必起第二个 daemon 生命周期即可复核推导）。
PH_SH=/tmp/ti-probe-host.sh
sed -n '/^health_probe_host()/,/^}/p' "$INSTALL_SH" >"$PH_SH"
[ -s "$PH_SH" ]
assert "A10-probe-host-fn-extracted" $?
# shellcheck disable=SC1090
. "$PH_SH"
[ "$(health_probe_host '10.0.0.5:8420')" = '10.0.0.5' ]
assert "A10-specific-host-probed-directly" $?
[ "$(health_probe_host '0.0.0.0:8420')" = '127.0.0.1' ]
assert "A10-wildcard-host-falls-back" $?
[ "$(health_probe_host ':8420')" = '127.0.0.1' ]
assert "A10-empty-host-falls-back" $?
[ "$(health_probe_host '[::1]:8420')" = '[::1]' ]
assert "A10-ipv6-literal-kept" $?
[ "$(health_probe_host '[::]:8420')" = '127.0.0.1' ]
assert "A10-ipv6-wildcard-falls-back" $?

# ------------------------------------- A11 openssl 轨双轨验签（S14/H15）
# dind 形态即目标消费形态：无 cosign、无真 curl（只有 busybox wget）——
# openssl 轨是唯一可验轨。四套 staged release（测试私钥签出 .sig.pem）+
# 假 curl（github download URL → 本地目录，404 语义对齐真 curl）让
# install.sh --version 端到端跑 release 下载形态（钉定版本不经 latest
# 解析，无外网依赖）。
have openssl
assert "A11-openssl-present" $? 'dind 镜像需带 openssl（A11 前提；目标镜像 docker:29.8.1-dind 实证带 3.5.x）'
if have cosign; then
    # cosign 在场会让 cosign 轨优先于 openssl 轨，本组断言前提被破坏。
    assert "A11-cosign-absent-premise" 1 'cosign unexpectedly present'
else
    assert "A11-cosign-absent-premise" 0
fi

# 内嵌公钥一致性（仓库原件口径）：install.sh == upgrade.sh 逐字一致 + 指纹
# 注释行在 + **指纹自洽**（注释值 == 内嵌公钥 DER SHA-256——防换钥忘改注释）。
# 仓库原件内嵌的是**生产**公钥（2026-09-20 首配起）；staged release 用测试
# 私钥签名，故四套流程跑之前先做**夹具手术**：把 $TI_STAGE 两份脚本的内嵌
# 公钥替换为测试公钥（A11 测的是 openssl 轨机制，不测钥归属；生产配对由
# release.yml 的 embedded-pubkey gate 对 secret 校验）。抽取口径：锚定
# FLEETLY_RELEASE_PUBKEY= 赋值行（BEGIN 标记必然跟着赋值前缀，无法锚定
# 行首）到 END 标记行，再剥掉首行赋值前缀——闭引号独占一行是前提（脚本
# 公钥块注释有结构说明）。
extract_embedded_pubkey() { # <script>
    sed -n "/^FLEETLY_RELEASE_PUBKEY='/,/-----END PUBLIC KEY-----/p" "$1" |
        sed "1s/^FLEETLY_RELEASE_PUBKEY='//"
}
extract_embedded_pubkey "$INSTALL_SH" >/tmp/ti-pub-install.pem
extract_embedded_pubkey "$UPGRADE_SH" >/tmp/ti-pub-upgrade.pem
openssl pkey -in "$TEST_SIGN_KEY" -pubout -out /tmp/ti-pub-testderived.pem
cmp -s /tmp/ti-pub-install.pem /tmp/ti-pub-upgrade.pem
assert "A11-embedded-key-consistent-install-upgrade" $?
grep -q 'fingerprint-sha256: ' "$INSTALL_SH" && grep -q 'fingerprint-sha256: ' "$UPGRADE_SH"
assert "A11-fingerprint-comment-present" $?
_fp_comment=$(grep -o 'fingerprint-sha256: [0-9a-f]*' "$INSTALL_SH" | head -1 | cut -d' ' -f2)
_fp_actual=$(openssl pkey -pubin -in /tmp/ti-pub-install.pem -outform DER |
    openssl dgst -sha256 | sed 's/^.*)= *//')
[ "$_fp_comment" = "$_fp_actual" ]
assert "A11-embedded-fingerprint-selfconsistent" $?

# 夹具手术：staged 副本内嵌公钥 ← 测试公钥（保持赋值行/闭引号结构；原闭引号
# 行被注入块取代——tail 起点跳过它，并以守卫确认其确为独占一行的一撇）。
inject_test_pubkey() { # <script>
    _s=$1
    _start=$(grep -n "^FLEETLY_RELEASE_PUBKEY='" "$_s" | head -1 | cut -d: -f1)
    _end=$(grep -n -- '-----END PUBLIC KEY-----' "$_s" | head -1 | cut -d: -f1)
    [ -n "$_start" ] && [ -n "$_end" ] && [ "$_end" -gt "$_start" ] || return 1
    [ "$(sed -n "$((_end + 1))p" "$_s")" = "'" ] || return 1
    {
        head -n $((_start - 1)) "$_s"
        printf "FLEETLY_RELEASE_PUBKEY='"
        cat /tmp/ti-pub-testderived.pem
        printf "'\n"
        tail -n +$((_end + 2)) "$_s"
    } >"$_s.new" && mv "$_s.new" "$_s"
}
inject_test_pubkey "$INSTALL_SH"
assert "A11-staged-surgery-install" $?
inject_test_pubkey "$UPGRADE_SH"
assert "A11-staged-surgery-upgrade" $?
extract_embedded_pubkey "$INSTALL_SH" | cmp -s - /tmp/ti-pub-testderived.pem
assert "A11-staged-embeds-test-key" $?

SIG_TAG='v0.1.0-sigtest'
SIG_ASSET="fleetly_${SIG_TAG}_linux_amd64.tar.gz"

# make_staged_release <dir> <mode> — 构造一套本地 release：
#   good     = checksums + .sig.pem（openssl 轨正例）；
#   tampered = 签名后向 checksums 追加 1 字节——asset 哈希行未动、内部自
#              一致，校验和层必过（正是威胁模型：同 origin 重打包自一致
#              投毒），签名层必须炸；
#   nosig    = 仅 checksums（双签名产物均 404 → 历史版本兼容警示路径）；
#   nopem    = checksums + 假 .sig bundle、无 .sig.pem（其一可用但本机无
#              cosign → 双轨全部不可验 → 降级即死）。
make_staged_release() {
    _d=$1
    _mode=$2
    mkdir -p "$_d"
    tar -czf "$_d/$SIG_ASSET" -C "$STAGE_BIN" fleetlyd fleetly
    (cd "$_d" && sha256sum "$SIG_ASSET" > checksums.txt)
    case "$_mode" in
    good | tampered)
        openssl dgst -sha256 -sign "$TEST_SIGN_KEY" \
            -out "$_d/checksums.txt.sig.pem" "$_d/checksums.txt"
        if [ "$_mode" = 'tampered' ]; then
            printf 'x' >>"$_d/checksums.txt"
        fi
        ;;
    nopem)
        : >"$_d/checksums.txt.sig" # 内容任意：无 cosign 时不会被验
        ;;
    nosig) ;;
    esac
}
make_staged_release /tmp/ti-rel-good good
make_staged_release /tmp/ti-rel-tampered tampered
make_staged_release /tmp/ti-rel-nosig nosig
make_staged_release /tmp/ti-rel-nopem nopem

# 假 curl：github release download URL → $TI_FAKE_REL_DIR 本地目录；其余
# URL 转发 $TI_REAL_CURL（dind 无真 curl，转发目标空即模拟不可达）。404
# 语义对齐真 curl：-w '%{http_code}' 输出 404；带 -f 时退出码 22。
FAKE_CURL_BIN=/tmp/ti-fakebin-curl
mkdir -p "$FAKE_CURL_BIN"
cat >"$FAKE_CURL_BIN/curl" <<'FAKECURL'
#!/bin/sh
_rel=${TI_FAKE_REL_DIR:-}
_real=${TI_REAL_CURL:-}
_out='' _w=0 _f=0 _url='' _prev=''
for _a in "$@"; do
    case "$_prev" in
    -o) _out=$_a ;;
    -w) _w=1 ;;
    esac
    case "$_a" in
    -f) _f=1 ;;
    http://* | https://*) _url=$_a ;;
    esac
    _prev=$_a
done
case "$_url" in
https://github.com/*/releases/download/*)
    _base=${_url##*/}
    if [ -n "$_rel" ] && [ -f "$_rel/$_base" ]; then
        if [ -n "$_out" ]; then
            cat "$_rel/$_base" >"$_out"
        else
            cat "$_rel/$_base"
        fi
        [ "$_w" -eq 1 ] && printf '200'
        exit 0
    fi
    [ -n "$_out" ] && : >"$_out"
    [ "$_w" -eq 1 ] && printf '404'
    [ "$_f" -eq 1 ] && exit 22
    exit 0
    ;;
esac
if [ -n "$_real" ]; then
    exec "$_real" "$@"
fi
[ "$_w" -eq 1 ] && printf '000'
exit 7
FAKECURL
chmod +x "$FAKE_CURL_BIN/curl"

# sig_install_run <rel-dir> <log> [extra install.sh args...] — 假 origin
# （fake curl）跑 install.sh 的 release 下载形态。
sig_install_run() {
    _rel=$1
    _log=$2
    shift 2
    TI_FAKE_REL_DIR="$_rel" TI_REAL_CURL='' PATH="$FAKE_CURL_BIN:$PATH" \
        sh "$INSTALL_SH" --version "$SIG_TAG" --no-systemd "$@" >"$_log" 2>&1
}

# ① 正例：无 cosign（干净 VPS 主路径）→ openssl 轨验签通过 + 安装成功。
sig_install_run /tmp/ti-rel-good "$SIG_OK_LOG"
RC=$?
assert "A11-openssl-track-install-rc0" "$RC" "rc=$RC"
grep -q 'signature: openssl dgst verify OK' "$SIG_OK_LOG"
assert "A11-openssl-track-verify-ok-line" $?
grep -q 'checksum: sha256 OK' "$SIG_OK_LOG"
assert "A11-openssl-track-checksum-ok-line" $?
[ -x /opt/fleetly/bin/fleetlyd ]
assert "A11-openssl-track-binary-installed" $?

# ② 篡改：内部自一致的 checksums 追加 1 字节——校验和层必过（正是要防
#    的同 origin 自一致重打包），openssl 验签必须 die。
sig_install_run /tmp/ti-rel-tampered "$SIG_TAMPER_LOG"
RC=$?
[ "$RC" -ne 0 ]
assert "A11-tampered-checksums-refused" $? "rc=$RC (want non-zero)"
grep -q 'checksum: sha256 OK' "$SIG_TAMPER_LOG"
assert "A11-tampered-passed-checksum-layer" $?
grep -q 'openssl signature verification FAILED' "$SIG_TAMPER_LOG"
assert "A11-tampered-caught-by-signature" $?

# ③ 双签名产物均缺（S14 之前的历史版本）：兼容警示路径，安装继续。
sig_install_run /tmp/ti-rel-nosig "$SIG_NOSIG_LOG"
RC=$?
assert "A11-nosig-compat-install-rc0" "$RC" "rc=$RC"
grep -q 'degraded compat path' "$SIG_NOSIG_LOG"
assert "A11-nosig-compat-warned" $?

# ④ 仅 .sig bundle（.sig.pem 缺）且本机无 cosign：双轨全部不可验 → die。
sig_install_run /tmp/ti-rel-nopem "$SIG_NOPEM_LOG"
RC=$?
[ "$RC" -ne 0 ]
assert "A11-no-pem-no-cosign-refused" $? "rc=$RC (want non-zero)"
grep -q 'no signature verifiable on this host' "$SIG_NOPEM_LOG"
assert "A11-no-pem-no-cosign-reason-echoed" $?

# ⑤ --skip-signature-verify 显式跳过位保留（调试用途，警告不变）。
sig_install_run /tmp/ti-rel-nopem "$SIG_SKIP_LOG" --skip-signature-verify
RC=$?
assert "A11-skip-flag-installs" "$RC" "rc=$RC"
grep -q 'SKIPPED by --skip-signature-verify' "$SIG_SKIP_LOG"
assert "A11-skip-flag-warns" $?

# --------------------------------------------------------------- A9 卸载
sh "$UNINSTALL_SH" >"$UNINSTALL_LOG" 2>&1
RC=$?
assert "A9-uninstall-rc0" "$RC" "rc=$RC"
[ ! -e /opt/fleetly/bin/fleetlyd ]
assert "A9-binary-gone" $?
[ ! -e /opt/fleetly/etc/config.yaml ]
assert "A9-config-gone" $?
[ ! -e /usr/local/bin/fleetly ]
assert "A9-symlink-gone" $?
[ ! -e /etc/systemd/system/fleetlyd.service ]
assert "A9-unit-gone" $?
[ -d /var/lib/fleetly ]
assert "A9-data-kept" $?
grep -q 'KEPT' "$UNINSTALL_LOG"
assert "A9-data-kept-echoed" $?
grep -q 'swarm leave --force' "$UNINSTALL_LOG"
assert "A9-swarm-leave-hint" $?

sh "$UNINSTALL_SH" --purge >>"$UNINSTALL_LOG" 2>&1
RC=$?
assert "A9-purge-rc0" "$RC" "rc=$RC"
[ ! -d /var/lib/fleetly ]
assert "A9-data-purged" $?

# ------------------------------------- A13 --base-domain 全链（E1-1 正路径）
# 此处 A9 已 purge：config 与数据根全新——--base-domain 安装写出含
# base_domain 的 config，报告含三平台子域行 + DNS 提示 + 8423 端口行；
# 随后以另一域名重装（config 存在被保留）→ 显式警示 base_domain 未写入；
# 最后词法校验负路径（带斜杠的伪域名）。
BD_LOG="/tmp/ti-base-domain.log"
BD_KEEP_LOG="/tmp/ti-base-domain-keep.log"
BD_VAL_LOG="/tmp/ti-base-domain-invalid.log"
sh "$INSTALL_SH" --bin-dir "$STAGE_BIN" --no-systemd --base-domain platform.test >"$BD_LOG" 2>&1
RC=$?
assert "A13-base-domain-install-rc0" "$RC" "rc=$RC"
grep -q 'base_domain: "platform.test"' /opt/fleetly/etc/config.yaml
assert "A13-config-has-base-domain-key" $?
grep -q 'ctrl.platform.test' "$BD_LOG"
assert "A13-report-ctrl-subdomain" $?
grep -q 'registry.platform.test' "$BD_LOG"
assert "A13-report-registry-subdomain" $?
grep -q 'console.platform.test' "$BD_LOG"
assert "A13-report-console-subdomain" $?
grep -q 'fleetly domains verify' "$BD_LOG"
assert "A13-report-dns-verify-hint" $?
grep -q '8423/tcp ingress-cfg-tls' "$BD_LOG"
assert "A13-report-8423-port-line" $?
# 安装器最小集纪律：registry/join 键不由安装器写出（消费方票据接线）。
grep -q -e '^registry:' -e 'token_rotate' -e 'auth_file' /opt/fleetly/etc/config.yaml
[ $? -ne 0 ]
assert "A13-config-minimal-keyset" $?
# 已有 config + --base-domain：保留 + 显式警示（不静默覆盖、不静默丢弃）。
CFG_SHA_BD=$(sha256sum /opt/fleetly/etc/config.yaml | awk '{print $1}')
sh "$INSTALL_SH" --bin-dir "$STAGE_BIN" --no-systemd --base-domain other.test >"$BD_KEEP_LOG" 2>&1
RC=$?
assert "A13-keep-config-install-rc0" "$RC" "rc=$RC"
grep -q 'base_domain was NOT written' "$BD_KEEP_LOG"
assert "A13-keep-config-warns" $?
[ "$(sha256sum /opt/fleetly/etc/config.yaml | awk '{print $1}')" = "$CFG_SHA_BD" ]
assert "A13-keep-config-byte-preserved" $?
# 词法校验负路径：带斜杠的伪域名拒绝并回显原因。
sh "$INSTALL_SH" --bin-dir "$STAGE_BIN" --no-systemd --base-domain 'bad/domain' >"$BD_VAL_LOG" 2>&1
RC=$?
[ "$RC" -ne 0 ]
assert "A13-invalid-domain-rejected" $? "rc=$RC (want non-zero)"
grep -q 'invalid --base-domain' "$BD_VAL_LOG"
assert "A13-invalid-domain-reason-echoed" $?

finish
