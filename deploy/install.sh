#!/bin/sh
# deploy/install.sh — fleetly 一条命令安装器（T2.1）。
#
# 目标：干净 Linux VPS（amd64/arm64，root）上一条命令装出可运行平台：
#   curl -fsSL https://fleetly.dev/install.sh | sh -
# 三种获取形态（都经完整性校验或本地直取）：
#   1. --version vX.Y.Z   从 GitHub releases 下载（checksums 必验；签名双轨
#                         必验其一——S14/H15：cosign bundle 轨优先，cosign
#                         缺席（干净 VPS 常态）走 openssl 内嵌公钥轨；双轨全
#                         部不可验即拒绝（降级即死），仅历史版本（release 未附
#                         任何签名产物）保留带显著警示的兼容路径；
#                         --skip-signature-verify 供调试显式跳过）
#   2. --bin-dir <dir>    离线/开发形态：直接使用目录内已放好的 fleetlyd/fleetly
#   3. 缺省               latest stable release
# 引擎门禁（architecture §4.2，不满足即拒绝并输出原因与建议）：
#   - Docker Engine >= 29.8.1
#   - iptables(legacy) 后端（nftables-only 暂不支持 Swarm 节点）
# 安装动作：/opt/fleetly（bin+etc）、/var/lib/fleetly（数据根）、
#   隐式 docker swarm init（advertise-addr 优先私网 IP）、systemd unit
#   （enable + start + 健康等待；--no-systemd 打印手动启动命令）。
# 卸载见同目录 uninstall.sh；dind 验收见 test-install.sh 与 README.md。
#
# 兼容性约束：POSIX sh（busybox ash / dash / bash 均可跑）——不使用
# bashism（数组、[[ ]]、local、$'' 等）。

set -eu

# ------------------------------------------------------------------ 常量
FLEETLY_PREFIX='/opt/fleetly'
FLEETLY_BIN_DIR="$FLEETLY_PREFIX/bin"
FLEETLY_ETC_DIR="$FLEETLY_PREFIX/etc"
FLEETLY_DATA_DIR='/var/lib/fleetly'
FLEETLY_LINK_DIR='/usr/local/bin'
FLEETLY_UNIT_PATH='/etc/systemd/system/fleetlyd.service'
FLEETLY_GITHUB_REPO='fleetlyrun/fleetly'
FLEETLY_MIN_ENGINE='29.8.1'
FLEETLY_LOG_FILE='/var/log/fleetlyd.log'

# ------------------------------------------- release 签名公钥（openssl 轨）
# S14/H15 双轨验签的兜底轨：cosign 缺席（干净 VPS 常态）时，用本内嵌公钥经
# openssl 验 checksums.txt 的 detached 签名（checksums 只防传输损坏，防不了
# release 侧同源自一致的重打包投毒——签名才绑密钥）。
# 算法裁决：RSA-2048 + SHA-256（openssl dgst 轨）。ed25519 更现代但
# `openssl dgst` 不支持 Ed 系签名（须 pkeyutl -rawin，且 1.1.1/3.x 参数面
# 分裂；实测 OpenSSL 4.0.2 的 dgst -sign 对 ed25519 仍报 unsupported），
# 而 dgst -sha256 -sign/-verify 自 OpenSSL 1.0.x 起全版本命令面一致——
# 兼容性优先（S14 方案冻结口径）。
# fingerprint-sha256: 929f85cfef85a5df55586bfbd3f225d4568aa7905192a7f904be09d78b2cfcf9
# 轮换：生成新密钥对 → 替换本变量与上面指纹行（install.sh/upgrade.sh 两侧
# 必须同步，test-install.sh A11 断言两侧逐字一致）→ 发布新安装器；旧 release
# 的历史 .sig.pem 用旧公钥仍可验（验失败提示升级安装器即轮换语义）。
# 结构注意：起始/结束引号各独占一行——公钥块保持干净 PEM 行。抽取比对
# 口径（test-install.sh A11 与 release.yml embedded-pubkey gate）：锚定
# FLEETLY_RELEASE_PUBKEY= 赋值行到 END 标记行、剥掉首行赋值前缀——替换
# 密钥时保持该形状，否则一致性断言会红。
# 当前内嵌 **生产**密钥（2026-09-20 首配，v0.1.0 发布前；私钥仅存 repo
# secret FLEETLY_RELEASE_KEY，配置与轮换流程见 deploy/README.md「release
# 签名密钥（openssl 轨）」）。release.yml 的 embedded-pubkey gate 强制校验
# 本块与 secret 配对——换 secret 忘换本块的 release 会被拦下。
FLEETLY_RELEASE_PUBKEY='-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEArGQp1Sv65HxB7kMVKcsf
78orC+mgEf0rGZ3SozVtZrXjgDONDSTjMHxojmn44ZsLyz1wE825qSlRQniLZa79
9lFykr18xMeFmdvXIfW/EhMKqy9CEXr+vLYMrSSps+m/pol/LIsmHicndDFZNOnj
jqQ/G48fep3Buohi0dCSxXdceAPDwG75hirqXifaY7tt7rV/RgrpTO0qdtaIWw0N
BAJmEDFomEH7Sgm1zVom73OLqkgKe5qCItnfPu2SJdBVDNPLsrxzmGZWC1EOtpHV
L79PwgMLPHGVDDt7rMdFwHSdvoHikWvGODX7cj7CWVhs7X+0I82HfIOqqXUWQDH6
WwIDAQAB
-----END PUBLIC KEY-----
'

# ------------------------------------------------------------------ 参数
FLEETLY_VERSION=''
BIN_DIR_SRC=''
HTTP_ADDR='0.0.0.0:8420'
BASE_DOMAIN=''
NO_SYSTEMD=0
SKIP_SIG=0
HARDEN_FIREWALL=0

log()  { printf '[install] %s\n' "$*"; }
warn() { printf '[install] WARNING: %s\n' "$*"; }
die()  { printf '[install] ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'USAGE'
usage: install.sh [options]

options:
  --version vX.Y.Z          install this release from GitHub releases
  --bin-dir <dir>           offline/dev form: use fleetlyd+fleetly already in <dir>
  --http-addr <host:port>   HTTP face bind address (default 0.0.0.0:8420)
  --base-domain <domain>    optional platform base DNS name (multi-node,
                            e.g. example.com): derives the ctrl/registry/
                            console subdomains, writes base_domain into the
                            generated config and adds the platform-domain
                            report section. Omit for the unchanged v0.1
                            single-node install (multi-node join REQUIRES it)
  --no-systemd              skip systemd unit/enable/start; print manual start command
  --skip-signature-verify   skip signature verification (cosign or openssl
                            track; debug only)
  --harden-firewall         reserved: firewall hardening lands in a later stage
  -h, --help                this help

engine gate (architecture design doc section 4.2):
  Docker Engine >= 29.8.1 and iptables(legacy) backend are required;
  nftables-only hosts are rejected.
USAGE
}

# ------------------------------------------------------------- 工具函数
have() { command -v "$1" >/dev/null 2>&1; }

# version_ge <a> <b>：点分数字版本比较（a >= b 为真）。缺失段按 0，
# 非数字后缀剥离（如 29.8.1-rc1 → 29.8.1）。
version_ge() {
    _va=${1:-}
    _vb=${2:-}
    while :; do
        _fa=${_va%%.*}
        _fb=${_vb%%.*}
        _fa=${_fa%%[!0-9]*}
        _fb=${_fb%%[!0-9]*}
        [ -n "$_fa" ] || _fa=0
        [ -n "$_fb" ] || _fb=0
        if [ "$_fa" -ne "$_fb" ]; then
            [ "$_fa" -gt "$_fb" ] && return 0
            return 1
        fi
        case "$_va" in *.*) _va=${_va#*.} ;; *) _va='' ;; esac
        case "$_vb" in *.*) _vb=${_vb#*.} ;; *) _vb='' ;; esac
        [ -z "$_va" ] && [ -z "$_vb" ] && return 0
    done
}

http_fetch() { # <url> <outfile>   （outfile 为 - 时写 stdout）
    _url=$1
    _out=$2
    if have curl; then
        curl -fsSL --retry 3 -o "$_out" "$_url"
    elif have wget; then
        wget -q -T 30 -O "$_out" "$_url"
    else
        die 'no http client found (need curl or wget)'
    fi
}

# fetch_release_sig <url> <outfile> — 签名产物下载（404 感知，S14/H15）：
#   返回 0 = 下载成功；返回 1 = 404（release 未附该签名产物——历史版本的
#   兼容路径信号）；其他失败（网络错、5xx、截断）= die——checksums.txt 刚从
#   同一 origin 成功取得，签名产物此刻取不到更可能是劫持/投毒面，fail
#   closed 不降级。curl 形态经 -w '%{http_code}' 精确分类（不带 -f：404 也是
#   正常 HTTP 应答）；wget-only 形态下载失败后用 -S 状态行探测（busybox
#   wget -S 的状态行走 stderr），探测不出即 die。
fetch_release_sig() {
    _url=$1
    _out=$2
    if have curl; then
        _code=$(curl -sSL --retry 2 -o "$_out" -w '%{http_code}' "$_url" 2>/dev/null) || _code='000'
        case "$_code" in
        2??) return 0 ;;
        404) rm -f "$_out"; return 1 ;;
        *) die "signature artifact download failed (HTTP $_code): $_url" ;;
        esac
    fi
    if wget -q -T 30 -O "$_out" "$_url" 2>/dev/null; then
        return 0
    fi
    rm -f "$_out"
    _st=$(wget -q -S -T 30 -O /dev/null "$_url" 2>&1 |
        sed -n '1s/^[[:space:]]*HTTP\/[0-9.]*[[:space:]]*\([0-9][0-9][0-9]\).*/\1/p')
    [ "$_st" = '404' ] && return 1
    die "signature artifact download failed (HTTP ${_st:-unknown}): $_url"
}

# verify_release_sig_pem <sigfile> <datafile> — openssl 轨验签（RSA-2048 /
# SHA-256，dgst 轨）。POSIX sh 无进程替换——内嵌公钥先落临时文件再验
# （变量自带收尾换行，用 %s 不再补行）；验签输出（Verified OK）静音，
# 成败经退出码表达。
verify_release_sig_pem() {
    _pub="$TMPD/release-pubkey.pem"
    printf '%s' "$FLEETLY_RELEASE_PUBKEY" > "$_pub"
    openssl dgst -sha256 -verify "$_pub" -signature "$1" "$2" >/dev/null 2>&1
}

# validate_base_domain <domain> — --base-domain 值校验（V2-7；宽松 DNS 名
# 判定，设计 §2.2：值是三平台子域的后缀，宁拒误配不放垃圾进配置）。只做
# 词法判定（字符集 [A-Za-z0-9.-]、至少一个点、无空标签、无首尾点、长度
# <=253）；DNS 可解析性与证书签发可达性是安装后的运维事实，不在此判。
validate_base_domain() { # <domain>
    _d=$1
    case "$_d" in
    '') die 'base-domain must be a DNS name (e.g. example.com)' ;;
    esac
    case "$_d" in
    *[!A-Za-z0-9.-]*)
        die "invalid --base-domain '$_d': only letters, digits, dots and hyphens are allowed"
        ;;
    esac
    case "$_d" in
    .* | *. | *..*)
        die "invalid --base-domain '$_d': no leading/trailing dot or empty label"
        ;;
    esac
    case "$_d" in
    *.*) : ;;
    *) die "invalid --base-domain '$_d': must contain at least one dot (e.g. example.com)" ;;
    esac
    [ "${#_d}" -le 253 ] || die "invalid --base-domain '$_d': exceeds 253 characters"
}

# health_probe_host <http-addr> — 从 --http-addr 推导健康探测目标 host
# （整改⑤）：探测必须打在 daemon 真实绑定的地址上——--http-addr 10.0.0.5:8420
# 时 daemon 只绑 10.0.0.5，硬编码 127.0.0.1 恒拒连（探测假死 60s 后 die，
# 而 systemctl enable --now 已成功——安装报红但平台在跑）。通配绑定
# （0.0.0.0 / :: / 空 host）回落 127.0.0.1；IPv6 括号形态原样保留（URL
# 词形 http://[::1]:port/... 合法）。
health_probe_host() { # <http-addr>
    _ph=${1%:*}
    case "$_ph" in
    '' | ':' | 0.0.0.0 | '::' | '[::]' | '[0.0.0.0]') printf '127.0.0.1' ;;
    *) printf '%s' "$_ph" ;;
    esac
}

# ---------------------------------------------------------------- 参数解析
while [ $# -gt 0 ]; do
    case "$1" in
    --version)
        [ $# -ge 2 ] || die '--version requires a value'
        FLEETLY_VERSION=$2
        shift 2
        ;;
    --bin-dir)
        [ $# -ge 2 ] || die '--bin-dir requires a value'
        BIN_DIR_SRC=$2
        shift 2
        ;;
    --http-addr)
        [ $# -ge 2 ] || die '--http-addr requires a value'
        HTTP_ADDR=$2
        shift 2
        ;;
    --base-domain)
        [ $# -ge 2 ] || die '--base-domain requires a value'
        BASE_DOMAIN=$2
        shift 2
        ;;
    --no-systemd)
        NO_SYSTEMD=1
        shift
        ;;
    --skip-signature-verify)
        SKIP_SIG=1
        shift
        ;;
    --harden-firewall)
        HARDEN_FIREWALL=1
        shift
        ;;
    -h | --help)
        usage
        exit 0
        ;;
    *)
        printf '[install] ERROR: unknown option: %s\n' "$1" >&2
        usage >&2
        exit 2
        ;;
    esac
done

if [ -n "$FLEETLY_VERSION" ] && [ -n "$BIN_DIR_SRC" ]; then
    die '--version and --bin-dir are mutually exclusive'
fi
case "$HTTP_ADDR" in
*:*) ;;
*) die "--http-addr must be host:port (got '$HTTP_ADDR')" ;;
esac
if [ -n "$BASE_DOMAIN" ]; then
    validate_base_domain "$BASE_DOMAIN"
fi

# ------------------------------------------------- 前置门禁（任何落盘动作之前）
# 1) 平台与权限：安装器只支持 Linux 宿主；需要 root（curl|sudo sh - 形态下
#    stdin 即脚本本体，因此不自动 re-exec sudo——明确拒绝并给出行内指引）。
[ "$(uname -s)" = 'Linux' ] || die "unsupported platform: $(uname -s) (this installer targets Linux only)"
[ "$(id -u)" = '0' ] || die 'must run as root — re-run via: curl -fsSL <install-url> | sudo sh -'

# 2) 架构。
case "$(uname -m)" in
x86_64) TARGET_ARCH='amd64' ;;
aarch64 | arm64) TARGET_ARCH='arm64' ;;
*) die "unsupported architecture: $(uname -m) (supported: x86_64/amd64, aarch64/arm64)" ;;
esac

# 3) 基础工具：docker 恒需；curl/wget 恒需（下载与 systemd 健康等待共用）；
#    tar/sha256sum 下载形态必经（bin-dir 形态多为开发机，通常也具备）。
have docker || die 'docker not found — install Docker Engine >= 29.8.1 first (https://docs.docker.com/engine/install/)'
have curl || have wget || die 'curl (or wget) not found — install curl and re-run'
have tar || die 'tar not found — install tar and re-run'
have sha256sum || die 'sha256sum not found — install coreutils/busybox and re-run'

# 4) 引擎版本门禁（architecture §4.2：Docker Engine >= 29.8.1）。
ENGINE_VERSION=$(docker version --format '{{.Server.Version}}' 2>/dev/null) ||
    die 'cannot read Docker Engine version — is the docker daemon running?'
if version_ge "$ENGINE_VERSION" "$FLEETLY_MIN_ENGINE"; then
    log "engine gate: Docker Engine $ENGINE_VERSION >= $FLEETLY_MIN_ENGINE: pass"
else
    die "engine gate: Docker Engine $ENGINE_VERSION < $FLEETLY_MIN_ENGINE — upgrade Docker Engine and re-run (architecture section 4.2: Docker 29.x has a breaking-change history, the floor is a hard gate)"
fi

# 5) iptables 后端门禁（architecture §4.2：nftables 暂不支持 Swarm 节点）。
#    判定顺序：默认 iptables 已是 legacy → 通过；否则存在可用的
#    iptables-legacy 用户态 → 通过；两者皆无/皆 nft → 拒绝。
IPTABLES_NOTE='unknown'
check_iptables_legacy() {
    if have iptables; then
        _v=$(iptables --version 2>/dev/null || true)
        case "$_v" in
        *'(legacy)'*)
            IPTABLES_NOTE="iptables default is legacy ($_v)"
            return 0
            ;;
        esac
    fi
    if have iptables-legacy; then
        _lv=$(iptables-legacy --version 2>/dev/null || true)
        case "$_lv" in
        *'(legacy)'*)
            IPTABLES_NOTE="iptables-legacy available ($_lv)"
            return 0
            ;;
        esac
    fi
    return 1
}
if check_iptables_legacy; then
    log "engine gate: iptables backend: pass ($IPTABLES_NOTE)"
else
    die "engine gate: iptables(legacy) backend not available — nftables-only hosts are not supported for Swarm nodes (architecture section 4.2). Fix hint (Debian/Ubuntu): update-alternatives --set iptables /usr/sbin/iptables-legacy, or run dockerd with legacy iptables userland installed"
fi

# --------------------------------------------------------------- 获取形态
TMPD=$(mktemp -d /tmp/fleetly-install.XXXXXX) || die 'mktemp failed'
cleanup() { rm -rf "$TMPD"; }
trap cleanup EXIT
trap 'exit 130' INT TERM

SRC_LABEL=''
if [ -n "$BIN_DIR_SRC" ]; then
    # ---- 形态 2：离线/开发（本地已备好二进制；仍打印版本信息）
    [ -d "$BIN_DIR_SRC" ] || die "bin-dir not found: $BIN_DIR_SRC"
    [ -x "$BIN_DIR_SRC/fleetlyd" ] || die "$BIN_DIR_SRC/fleetlyd missing or not executable"
    [ -x "$BIN_DIR_SRC/fleetly" ] || die "$BIN_DIR_SRC/fleetly missing or not executable"
    cp "$BIN_DIR_SRC/fleetlyd" "$TMPD/fleetlyd"
    cp "$BIN_DIR_SRC/fleetly" "$TMPD/fleetly"
    chmod 0755 "$TMPD/fleetlyd" "$TMPD/fleetly"
    SRC_LABEL="bin-dir:$BIN_DIR_SRC"
    log "source: local bin-dir ($SRC_LABEL)"
    BIN_VER=$("$TMPD/fleetly" version 2>/dev/null || true)
    [ -n "$BIN_VER" ] || BIN_VER='(version query unavailable)'
    log "binary version: $BIN_VER"
else
    # ---- 形态 1/3：GitHub releases（checksum 必验 + cosign 尽力而为）
    if [ -n "$FLEETLY_VERSION" ]; then
        case "$FLEETLY_VERSION" in
        v*) TAG=$FLEETLY_VERSION ;;
        *) TAG="v$FLEETLY_VERSION" ;;
        esac
        RELEASE_LABEL="$TAG (--version)"
    else
        log 'resolving latest stable release from GitHub...'
        LATEST=$(http_fetch "https://api.github.com/repos/$FLEETLY_GITHUB_REPO/releases/latest" - 2>/dev/null || true)
        TAG=$(printf '%s\n' "$LATEST" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
        [ -n "$TAG" ] || die 'cannot resolve latest release (GitHub API unreachable?) — retry, or pin with --version / use --bin-dir'
        RELEASE_LABEL="$TAG (latest)"
    fi
    ASSET="fleetly_${TAG}_linux_${TARGET_ARCH}.tar.gz"
    BASE_URL="https://github.com/$FLEETLY_GITHUB_REPO/releases/download/$TAG"
    log "downloading $ASSET"
    http_fetch "$BASE_URL/$ASSET" "$TMPD/$ASSET" ||
        die "download failed: $BASE_URL/$ASSET (check the release exists and the network)"
    http_fetch "$BASE_URL/checksums.txt" "$TMPD/checksums.txt" ||
        die "download failed: $BASE_URL/checksums.txt"
    # 双轨签名产物各自尝试下载（S14）：checksums.txt.sig = cosign keyless
    # bundle；checksums.txt.sig.pem = openssl 轨 detached 签名（内容是
    # openssl dgst 的裸 DER 签名，.pem 为产物链约定名）。非 404 失败 → die
    # （见 fetch_release_sig）；双 404 = release 未附签名产物（历史版本）→
    # 下方验证段的兼容警示路径。
    HAVE_BUNDLE_SIG=0
    HAVE_PEM_SIG=0
    if fetch_release_sig "$BASE_URL/checksums.txt.sig" "$TMPD/checksums.txt.sig"; then
        HAVE_BUNDLE_SIG=1
    fi
    if fetch_release_sig "$BASE_URL/checksums.txt.sig.pem" "$TMPD/checksums.txt.sig.pem"; then
        HAVE_PEM_SIG=1
    fi

    # 校验和必验（delivery-pipeline §2.4：curl|sh 完整性基线）。
    EXPECTED=$(grep -E "^[0-9a-f]{64}[[:space:]]+\*?$ASSET\$" "$TMPD/checksums.txt" | awk '{print $1}' | head -n 1)
    [ -n "$EXPECTED" ] || die "$ASSET not listed in checksums.txt — refusing to install unverified artifacts"
    ACTUAL=$(sha256sum "$TMPD/$ASSET" | awk '{print $1}')
    [ "$ACTUAL" = "$EXPECTED" ] ||
        die "checksum mismatch for $ASSET: expected $EXPECTED, got $ACTUAL"
    log "checksum: sha256 OK ($ACTUAL)"

    # 签名验证（S14/H15 双轨，降级即死）：
    #   - cosign 在且 bundle 在 → cosign verify-blob（keyless identity 约束
    #     不变，失败即中止）；
    #   - 否则 .sig.pem 在 → openssl 轨（内嵌公钥，失败即中止）；
    #   - 两者都不可验（如 cosign 缺席且 release 只附了 bundle）→ die：
    #     checksums 与产物同 origin，防不了 release 侧自一致重打包投毒——
    #     这正是 H15 的信任落差，不再 warn 降级；
    #   - 仅历史版本（双轨产物均未附）保留带显著警示的兼容路径。
    if [ "$SKIP_SIG" -eq 1 ]; then
        warn 'signature verification SKIPPED by --skip-signature-verify (debug only — do not use in production)'
    elif [ "$HAVE_BUNDLE_SIG" -eq 0 ] && [ "$HAVE_PEM_SIG" -eq 0 ]; then
        warn 'release carries NO signature artifacts (pre-S14 historical release) — signature verification SKIPPED (degraded compat path; releases carrying signatures are enforced strictly)'
    elif have cosign && [ "$HAVE_BUNDLE_SIG" -eq 1 ]; then
        cosign verify-blob \
            --bundle "$TMPD/checksums.txt.sig" \
            --certificate-identity-regexp "^https://github.com/$FLEETLY_GITHUB_REPO/" \
            --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
            "$TMPD/checksums.txt" ||
            die 'cosign signature verification FAILED — refusing to install'
        log 'signature: cosign verify-blob OK'
    elif [ "$HAVE_PEM_SIG" -eq 1 ]; then
        have openssl ||
            die 'openssl not found — cannot verify checksums.txt.sig.pem (install openssl — or cosign — and re-run)'
        verify_release_sig_pem "$TMPD/checksums.txt.sig.pem" "$TMPD/checksums.txt" ||
            die 'openssl signature verification FAILED — refusing to install (release key mismatch usually means a rotated key: update this installer from https://github.com/fleetlyrun/fleetly)'
        log 'signature: openssl dgst verify OK (embedded release key)'
    else
        die 'release carries no signature verifiable on this host (cosign absent and no checksums.txt.sig.pem) — install cosign for the bundle track, or update this installer; --skip-signature-verify exists for debug only'
    fi

    tar -xzf "$TMPD/$ASSET" -C "$TMPD" || die "extract $ASSET failed"
    [ -x "$TMPD/fleetlyd" ] || die "$ASSET does not contain an executable fleetlyd at archive root (release-artifact contract violation)"
    [ -x "$TMPD/fleetly" ] || die "$ASSET does not contain an executable fleetly at archive root (release-artifact contract violation)"
    SRC_LABEL="release:$TAG"
fi

# --------------------------------------------------------------- 安装动作
log "installing from $SRC_LABEL"

# 已有 systemd unit（升级/重装）先停服再换二进制。
WAS_INSTALLED=0
if [ -f "$FLEETLY_UNIT_PATH" ]; then
    WAS_INSTALLED=1
    if have systemctl && [ -d /run/systemd/system ]; then
        systemctl stop fleetlyd.service >/dev/null 2>&1 || true
    else
        warn 'previous unit found but systemd is not running — a manually started fleetlyd must be stopped by hand'
    fi
fi

mkdir -p "$FLEETLY_BIN_DIR" "$FLEETLY_ETC_DIR"
mkdir -p "$FLEETLY_DATA_DIR"
chmod 0755 "$FLEETLY_BIN_DIR" "$FLEETLY_ETC_DIR"
chmod 0700 "$FLEETLY_DATA_DIR"

# rm -f 先于放置：重装时旧二进制可能仍在运行（ETXTBSY），先摘链再落新。
rm -f "$FLEETLY_BIN_DIR/fleetlyd" "$FLEETLY_BIN_DIR/fleetly"
install -m 0755 "$TMPD/fleetlyd" "$FLEETLY_BIN_DIR/fleetlyd" ||
    { cp "$TMPD/fleetlyd" "$FLEETLY_BIN_DIR/fleetlyd" && chmod 0755 "$FLEETLY_BIN_DIR/fleetlyd"; }
install -m 0755 "$TMPD/fleetly" "$FLEETLY_BIN_DIR/fleetly" ||
    { cp "$TMPD/fleetly" "$FLEETLY_BIN_DIR/fleetly" && chmod 0755 "$FLEETLY_BIN_DIR/fleetly"; }

# CLI/daemon 入 PATH（卸载时清理，不留孤儿）。
mkdir -p "$FLEETLY_LINK_DIR"
ln -sfn "$FLEETLY_BIN_DIR/fleetly" "$FLEETLY_LINK_DIR/fleetly"
ln -sfn "$FLEETLY_BIN_DIR/fleetlyd" "$FLEETLY_LINK_DIR/fleetlyd"

# --------------------------------------------------------------- 配置生成
# 最小可用配置：HTTP 面 0.0.0.0:8420（或 --http-addr 覆盖）、gRPC 只绑回环、
# 数据路径全部落 /var/lib/fleetly。已存在的 config.yaml 保留（升级不覆盖
# 用户配置）。
if [ -f "$FLEETLY_ETC_DIR/config.yaml" ]; then
    log "config exists, keeping: $FLEETLY_ETC_DIR/config.yaml"
    if [ -n "$BASE_DOMAIN" ]; then
        warn "config exists and was KEPT — base_domain was NOT written; add 'base_domain: \"$BASE_DOMAIN\"' to $FLEETLY_ETC_DIR/config.yaml manually (see config-example.yaml)"
    fi
else
    cat > "$FLEETLY_ETC_DIR/config.yaml" <<EOF
# fleetlyd config — generated by deploy/install.sh (T2.1)
# 全量键位与注释口径见 config-example.yaml；此处只写显式落定的最小集，
# 其余键回落 internal/* 各 Config.Normalize 的平台默认。

# HTTP 面（REST/gateway + healthz + Console 托管）：绑定 0.0.0.0 对外提供
# 面板/API（公网部署请配合防火墙或反代，见安装报告端口暴露面）。
addr: "$HTTP_ADDR"

# gRPC 面（CLI/SDK 直连）：只绑回环——内部服务默认不暴露公网。
grpc:
  addr: "127.0.0.1:8421"

# 状态层与数据根。
state:
  db_path: "$FLEETLY_DATA_DIR/fleetly.db"

secrets:
  key_path: "$FLEETLY_DATA_DIR/fleetly.key"

build:
  cache_dir: "$FLEETLY_DATA_DIR/build-cache"
  artifacts_dir: "$FLEETLY_DATA_DIR/build-artifacts"

logs:
  dir: "$FLEETLY_DATA_DIR/fleetly-logs"

ingress:
  token_file: "$FLEETLY_DATA_DIR/fleetly-ingress.token"
  cert_dir: "$FLEETLY_DATA_DIR/fleetly-certs"

# git webhook 触发入口：bare 仓库根（webhook 拉源的 fetch 落点）。git push
# (SSH) 收包面已移除（ADR-0012），无监听端口。
git:
  root: "$FLEETLY_DATA_DIR/git"

logging:
  level: info
EOF
    if [ -n "$BASE_DOMAIN" ]; then
        # 平台域名（V2-7；E1 多节点设计 §2.2）：仅 --base-domain 显式给出
        # 时追加——缺省形态的生成文件与 v0.1 逐字节一致（E1-1 硬断言，
        # test-install.sh A12 金样钉住）。
        cat >> "$FLEETLY_ETC_DIR/config.yaml" <<EOF

# 平台域名（多节点；V2-7）。填写后控制面派生三平台子域
#（ctrl/registry/console.$BASE_DOMAIN）并启用 8423 配置端点 TLS 面；
# 多节点 join 启用时必填（D-MN-13）。
base_domain: "$BASE_DOMAIN"
EOF
    fi
    chmod 0644 "$FLEETLY_ETC_DIR/config.yaml"
    log "config written: $FLEETLY_ETC_DIR/config.yaml"
fi

# --------------------------------------------------- systemd unit（同源模板）
# 与 deploy/fleetlyd.service 逐字一致（含注释头；test-install.sh 做 diff 防
# 漂移）：systemd 环境写入 /etc/systemd/system/fleetlyd.service；
# --no-systemd 环境写入 /opt/fleetly/etc/ 参考副本。
write_unit() { # <dest>
    cat > "$1" <<'UNIT_EOF'
# fleetlyd.service — fleetly 控制面 systemd unit（T2.1 参考模板）。
#
# 安装期由 deploy/install.sh 内嵌的同源 heredoc 生成：systemd 环境写入
# /etc/systemd/system/fleetlyd.service，--no-systemd 环境写入
# /opt/fleetly/etc/fleetlyd.service（参考副本，供手工环境对照）。
# 本文件与 install.sh 内嵌内容保持一致——修改任一侧必须同步另一侧，
# deploy/test-install.sh 会对两侧做 diff 断言防漂移。
#
# 设计依据：docs/design/2026-09-17-architecture.md §2.6（运行时 = 单节点
# Swarm）、§4.2（安全默认基线）；硬约束 = daemon 经本地 docker.sock 管理
# Swarm 集群，故 unit 不指定 User（root 运行——专用用户 + docker 组收敛
# 属后续加固，见 deploy/README.md 已知边界）。

[Unit]
Description=fleetly control plane daemon (fleetlyd)
Documentation=https://github.com/fleetlyrun/fleetly
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=exec
# 数据根：daemon 的工作目录（SQLite/主密钥/git bare 仓库/构建缓存/日志/
# 证书等都落这里；卸载默认保留的就是这个目录）。
WorkingDirectory=/var/lib/fleetly
ExecStart=/opt/fleetly/bin/fleetlyd -c /opt/fleetly/etc/config.yaml
Restart=on-failure
RestartSec=5s
# SIGTERM → lynx Runner 优雅关停（e2e/smoke.sh 断言 5 的退出码 0 契约）。
KillSignal=SIGTERM
TimeoutStopSec=30s

# ---- 适度加固（逐项注释；过度加固会让排障变黑盒，故不启用全量 sandbox）----
# 阻断 setuid/setgid 提权链（daemon 无特权子进程需求）。
NoNewPrivileges=true
# /tmp 私有化（防符号链接攻击与跨服务窥探）。
PrivateTmp=true
# 整个文件系统只读挂载；唯一可写面 = 数据根（ReadWritePaths 显式放行）。
ProtectSystem=strict
ReadWritePaths=/var/lib/fleetly
# 用户 home 不可见（平台数据全部在 /var/lib/fleetly，不需要 home）。
ProtectHome=true
# 内核参数/模块/控制组只读或禁改。
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
# 仅保留 daemon 实际需要的地址族（TCP 双栈 + docker.sock 的 AF_UNIX）。
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
# 常规加固位：禁 SUID/SGID 文件创建、固定 personality、禁止可写可执行内存映射。
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true

[Install]
WantedBy=multi-user.target
UNIT_EOF
}

write_unit "$FLEETLY_ETC_DIR/fleetlyd.service"
chmod 0644 "$FLEETLY_ETC_DIR/fleetlyd.service"

# ------------------------------------------------------- 隐式 swarm init
# architecture §2.6：单节点也是单节点 Swarm，安装时隐式 init（已初始化则跳过）。
# advertise-addr 选取：优先私网 IP（RFC1918），排除 loopback/link-local 与
# docker 网桥接口；无私网 → 公网 IP 并在安装报告警示暴露面（§4.2 底座端口加固）。
# 实现注意：本函数在当前 shell 执行（非 $() 子 shell）——公网判定经标记文件
# 传出，避免子 shell 变量作用域吞掉 ADV_IS_PUBLIC。
ADV_IS_PUBLIC=0
pick_advertise_addr() {
    _cands_file="$TMPD/adv-candidates"
    _public_marker="$TMPD/adv-public"
    if have ip; then
        ip -o -4 addr show scope global 2>/dev/null | awk '{print $2, $4}' > "$_cands_file" || true
    fi
    if [ ! -s "$_cands_file" ]; then
        hostname -i 2>/dev/null | awk '{print $1}' | sed 's/^/unknown /' > "$_cands_file" || true
    fi
    _pick=''
    _pub=''
    while read -r _if _addr; do
        [ -n "${_addr:-}" ] || continue
        _ip=${_addr%%/*}
        case "$_ip" in
        127.* | 169.254.*) continue ;;
        esac
        # docker 网桥接口上的地址不作为 advertise 候选（docker0/自定义网桥）。
        case "$_if" in
        docker* | br-* | *gwbridge*) continue ;;
        esac
        case "$_ip" in
        10.* | 192.168.*)
            [ -z "$_pick" ] && _pick=$_ip
            ;;
        172.*)
            _o2=${_ip#*.}
            _o2=${_o2%%.*}
            if [ "$_o2" -ge 16 ] && [ "$_o2" -le 31 ]; then
                [ -z "$_pick" ] && _pick=$_ip
            else
                [ -z "$_pub" ] && _pub=$_ip
            fi
            ;;
        *)
            [ -z "$_pub" ] && _pub=$_ip
            ;;
        esac
    done < "$_cands_file"
    if [ -n "$_pick" ]; then
        printf '%s\n' "$_pick"
        return 0
    fi
    if [ -n "$_pub" ]; then
        : > "$_public_marker"
        printf '%s\n' "$_pub"
        return 0
    fi
    return 1
}

SWARM_MODE=''
ADV=''
rm -f "$TMPD/adv-public" 2>/dev/null || true
if ! pick_advertise_addr > "$TMPD/adv-choice"; then
    die 'no usable advertise-addr candidate (no global IPv4 address found)'
fi
ADV=$(head -n 1 "$TMPD/adv-choice")
if [ -f "$TMPD/adv-public" ]; then
    ADV_IS_PUBLIC=1
fi
if [ "$(docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null || echo unknown)" = 'active' ]; then
    SWARM_MODE='already-active'
    ADV=$(docker info --format '{{.Swarm.NodeAddr}}' 2>/dev/null || true)
    ADV=${ADV%%:*}
    log "swarm already active (advertise-addr $ADV) — init skipped"
else
    if [ "$ADV_IS_PUBLIC" -eq 1 ]; then
        warn "advertise-addr is a PUBLIC address ($ADV) — swarm control ports (2377/tcp, 7946/tcp+udp, 4789/udp) would be internet-exposed; restrict with host firewall NOW (architecture section 4.2)"
    else
        log "advertise-addr: $ADV (private)"
    fi
    docker swarm init --advertise-addr "$ADV" >/dev/null ||
        die "docker swarm init failed (advertise-addr $ADV) — check 'docker info' and firewall rules"
    SWARM_MODE='initialized'
    log "swarm initialized (advertise-addr $ADV)"
fi

# 报告用统一分类（already-active 形态的 NodeAddr 也按模式归类）。
classify_adv() { # <ip>
    case "$1" in
    10.* | 192.168.*) return 1 ;;
    172.*)
        _o2=${1#*.}
        _o2=${_o2%%.*}
        if [ "$_o2" -ge 16 ] && [ "$_o2" -le 31 ]; then return 1; fi
        return 0
        ;;
    esac
    return 0
}
classify_adv "$ADV" && ADV_IS_PUBLIC=1 || ADV_IS_PUBLIC=0

# ------------------------------------------------------------ systemd 分支
SYSTEMD_MODE='skipped'
if [ "$NO_SYSTEMD" -eq 1 ]; then
    log '--no-systemd: skipping unit install/enable/start'
    log "manual start: cd $FLEETLY_DATA_DIR && $FLEETLY_BIN_DIR/fleetlyd -c $FLEETLY_ETC_DIR/config.yaml"
    log "manual logs  : stdout (or redirect to $FLEETLY_LOG_FILE)"
else
    if have systemctl && [ -d /run/systemd/system ]; then
        write_unit "$FLEETLY_UNIT_PATH"
        chmod 0644 "$FLEETLY_UNIT_PATH"
        systemctl daemon-reload
        if systemctl enable --now fleetlyd.service; then
            SYSTEMD_MODE='enabled+started'
        else
            echo '---- systemctl status ----' >&2
            systemctl status fleetlyd.service --no-pager -l 2>&1 || true
            echo '---- journalctl tail ----' >&2
            journalctl -u fleetlyd.service -n 50 --no-pager 2>&1 || true
            die 'systemctl enable/start failed — see status/journal output above'
        fi
        # 健康等待：/healthz/liveness 轮询；探测 host 跟随 --http-addr 的
        # host 部分（整改⑤——非回环绑定时 127.0.0.1 恒拒连）；失败给出
        # journalctl 诊断指引。
        HEALTH_PORT=${HTTP_ADDR##*:}
        HEALTH_HOST=$(health_probe_host "$HTTP_ADDR")
        _deadline=$(( $(date +%s) + 60 ))
        _healthy=0
        while [ "$(date +%s)" -lt "$_deadline" ]; do
            if have curl; then
                curl -fsS --max-time 3 "http://$HEALTH_HOST:$HEALTH_PORT/healthz/liveness" >/dev/null 2>&1 && _healthy=1 && break
            else
                wget -q -T 3 -O /dev/null "http://$HEALTH_HOST:$HEALTH_PORT/healthz/liveness" 2>/dev/null && _healthy=1 && break
            fi
            sleep 2
        done
        if [ "$_healthy" -eq 1 ]; then
            log "health: /healthz/liveness 200 on $HEALTH_HOST:$HEALTH_PORT"
        else
            echo '---- systemctl status ----' >&2
            systemctl status fleetlyd.service --no-pager -l 2>&1 || true
            echo '---- journalctl tail ----' >&2
            journalctl -u fleetlyd.service -n 50 --no-pager 2>&1 || true
            die "fleetlyd did not become healthy within 60s — diagnose with: journalctl -u fleetlyd.service -f"
        fi
    else
        die 'systemd not detected on this host — re-run with --no-systemd (container/chroot environments)'
    fi
fi

# ---------------------------------------------------------------- 安装报告
# 端口面判定使用真实 bind host（整改⑤）：通配绑定 = PUBLIC；回环 =
# loopback only；具体地址 = 按该地址评估暴露面（不再是含糊的 as configured）。
HTTP_BIND_HOST=${HTTP_ADDR%:*}
case "$HTTP_BIND_HOST" in
'' | ':' | 0.0.0.0 | '::' | '[::]') HTTP_EXPOSURE='PUBLIC (wildcard bind)' ;;
127.0.0.1 | '[::1]' | localhost) HTTP_EXPOSURE='loopback only' ;;
*) HTTP_EXPOSURE="bound to $HTTP_BIND_HOST (evaluate exposure for this address)" ;;
esac
ADV_CLASS='private'
if [ "$ADV_IS_PUBLIC" -eq 1 ]; then
    ADV_CLASS='PUBLIC - restrict source IPs now'
fi

printf '\n'
printf '==================== fleetly install report ====================\n'
printf 'version         : %s\n' "${RELEASE_LABEL:-$SRC_LABEL}"
printf 'install root    : %s (bin+etc) / %s (data)\n' "$FLEETLY_PREFIX" "$FLEETLY_DATA_DIR"
printf 'engine          : %s (gate >= %s: pass)\n' "$ENGINE_VERSION" "$FLEETLY_MIN_ENGINE"
printf 'iptables        : %s\n' "$IPTABLES_NOTE"
printf 'swarm           : %s (advertise-addr %s, %s)\n' \
    "$SWARM_MODE" "$ADV" \
    "$([ "$ADV_IS_PUBLIC" -eq 1 ] && printf PUBLIC || printf private)"
printf '\n'
printf 'port exposure (section 4.2 baseline):\n'
printf '  %-22s %s\n' "8420/tcp http api+ui" "bound $HTTP_ADDR -> $HTTP_EXPOSURE"
printf '  %-22s %s\n' '8421/tcp grpc' 'bound 127.0.0.1 -> loopback only'
printf '  %-22s %s\n' '8422/tcp ingress-cfg' 'bound 0.0.0.0 (default) -> token-protected, restrict if possible'
if [ -n "$BASE_DOMAIN" ]; then
    printf '  %-22s %s\n' '8423/tcp ingress-cfg-tls' 'bound 0.0.0.0 -> token+TLS platform cert (multi-node Traefik config endpoint; MANAGER only, not via Traefik)'
fi
printf '  %-22s %s\n' '80,443/tcp ingress' 'traefik host ports -> PUBLIC (expected app entry)'
printf '  %-22s %s\n' '2377,7946,4789 swarm' "advertised on $ADV ($ADV_CLASS)"
if [ "$HARDEN_FIREWALL" -eq 1 ]; then
    printf '  %-22s %s\n' '--harden-firewall' 'reserved for a later stage — no iptables rules were written'
fi
if [ -n "$BASE_DOMAIN" ]; then
    # 平台子域与 DNS 指引（E1 多节点设计 §2.2）：A 记录矩阵 + join 向导
    # 的 DNS 步骤复述；这里只提示，不做任何 DNS/防火墙动作（平台不静默
    # 改用户基础设施）。
    printf '\n'
    printf 'platform domains (base domain %s):\n' "$BASE_DOMAIN"
    printf '  %-24s %s\n' "ctrl.$BASE_DOMAIN" 'A -> MANAGER only - fleetlyd config endpoint TLS 8423 (not via Traefik)'
    printf '  %-24s %s\n' "registry.$BASE_DOMAIN" 'A -> ALL nodes - Traefik -> fleetly-registry (zot)'
    printf '  %-24s %s\n' "console.$BASE_DOMAIN" 'A -> ALL nodes - Traefik -> gateway (8420)'
    printf '  %-24s %s\n' 'dns hint' 'app + platform A records on ALL nodes (TTL <= 300s); verify with: fleetly domains verify'
fi
printf '\n'
printf 'systemd         : %s\n' "$SYSTEMD_MODE"
printf 'bootstrap token : first start writes it to %s/bootstrap-token (0600, never logged — remove after first successful login)\n' "$FLEETLY_DATA_DIR"
printf 'cli             : FLEETLY_ADDR=127.0.0.1:8421 FLEETLY_TOKEN=<token> fleetly apps list\n'
printf 'uninstall       : sh uninstall.sh (keeps %s; --purge removes it)\n' "$FLEETLY_DATA_DIR"
printf '================================================================\n'

if [ "$ADV_IS_PUBLIC" -eq 1 ]; then
    warn 'advertise-addr is PUBLIC — review the port exposure section above before going live'
fi
log 'install done'
