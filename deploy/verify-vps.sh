#!/bin/sh
# verify-vps.sh — fleetly staging/dogfooding VPS 环境验证（T1-V2.1，交付 §2.2
# 「VPS 验证脚本化」）。在目标 VPS 上运行：安装前环境门（OS/资源/引擎/端口/
# 出网/可选 DNS 对照），只读不改动。安装门禁本体（Engine ≥29.8.1 +
# iptables(legacy)）仍由 install.sh 权威执行——本脚本是前置体检与排障加速器，
# 判定口径与 install.sh 对齐但输出更细。
#
# 用法：
#   sh verify-vps.sh                 # 环境体检（不含 DNS 对照）
#   sh verify-vps.sh dev.example.com # 追加 DNS 对照：域名应解析到本机公网 IP
#                                     #（LE HTTP-01 的前置；解析不到只 FAIL 该项）
set -u

failures=0
warns=0

ok()   { printf 'PASS  %s\n' "$1"; }
warn() { printf 'WARN  %s\n' "$1"; warns=$((warns+1)); }
fail() { printf 'FAIL  %s\n' "$1"; failures=$((failures+1)); }
info() { printf '      %s\n' "$1"; }

# ── OS 与内核 ────────────────────────────────────────────────
os_id=$(. /etc/os-release 2>/dev/null && printf '%s' "$ID")
os_ver=$(. /etc/os-release 2>/dev/null && printf '%s' "$VERSION_ID")
case "$os_id" in
  debian|ubuntu) ok "OS: $os_id $os_ver ($(uname -r)"")" ;;
  *) warn "OS: $os_id $os_ver — 未验证发行版（官方支持面：Debian/Ubuntu）" ;;
esac

# ── 资源（建议线 = warn，硬线 = fail）────────────────────────
cores=$(nproc 2>/dev/null || printf '0')
if [ "$cores" -ge 2 ]; then ok "CPU: ${cores} cores"; else fail "CPU: ${cores} cores (< 2)"; fi

mem_mb=$(free -m 2>/dev/null | awk '/^Mem:/{print $2}')
if [ -n "$mem_mb" ]; then
  if [ "$mem_mb" -ge 3500 ]; then ok "RAM: ${mem_mb}MB"
  elif [ "$mem_mb" -ge 2000 ]; then warn "RAM: ${mem_mb}MB（≥2000 可跑，<4000 建议线以下——平台+应用同机时留意水位）"
  else fail "RAM: ${mem_mb}MB (< 2000)"; fi
else
  warn "RAM: free -m 不可用，跳过"
fi

disk_gb=$(df -BG / 2>/dev/null | awk 'NR==2{gsub("G","",$4); print $4}')
if [ -n "$disk_gb" ]; then
  if [ "$disk_gb" -ge 20 ]; then ok "Disk: / free ${disk_gb}GB"; else fail "Disk: / free ${disk_gb}GB (< 20)"; fi
else
  warn "Disk: df 不可用，跳过"
fi

# ── 容器引擎（与 install.sh 门禁同口径的预检）─────────────────
if command -v docker >/dev/null 2>&1; then
  dv=$(docker --version 2>/dev/null | sed 's/[^0-9.]*\([0-9.]*\).*/\1/')
  info "docker: $dv"
  case "$dv" in
    29.*|3[0-9].*) ok "Docker Engine: $dv (>= 29.8.1)" ;;
    '') warn "Docker Engine: 版本解析失败（安装门禁会重新判定）" ;;
    *) fail "Docker Engine: $dv (< 29.8.1)" ;;
  esac
  if docker info >/dev/null 2>&1; then
    ok "docker daemon: reachable"
    if docker info 2>/dev/null | grep -q 'Swarm: active'; then
      warn "Swarm: already active（复用将跳过 init——dogfooding 基线建议干净机）"
    else
      ok "Swarm: inactive（install.sh 将初始化单节点 Swarm）"
    fi
  else
    fail "docker daemon: unreachable（systemctl start docker 后重试）"
  fi
else
  fail "Docker Engine: not installed（Debian/Ubuntu: curl -fsSL https://get.docker.com | sh）"
fi

# iptables legacy（install.sh A8 门禁同源判定）
if command -v iptables >/dev/null 2>&1; then
  if iptables --version 2>/dev/null | grep -q '(legacy)'; then
    ok "iptables: legacy mode"
    # 双后端残留检测（2026-09-20 VPS dogfooding 实证）：daemon 以 nft 后端首启后
    # 再切 legacy，残留的 nft 表仍挂在内核钩子上——宿主端口照常、DNAT 发布端口
    # （80/443）全死。修复 = nft flush ruleset + systemctl restart docker。
    if nft list table ip nat >/dev/null 2>&1; then
      fail "iptables: legacy mode BUT stale nft 'ip nat' table present（daemon 曾以 nft 后端启动——nft flush ruleset && systemctl restart docker 后重检）"
    else
      ok "iptables: no stale nft tables"
    fi
  else
    fail "iptables: $(iptables --version)（update-alternatives --set iptables /usr/sbin/iptables-legacy）"
  fi
else
  fail "iptables: not found"
fi

# ── 端口占用（平台面：80/443 入口；8420 网关；8423 预留；8424 git SSH 已移除，ADR-0012）──
for p in 80 443 8420 8423; do
  if ss -ltn 2>/dev/null | grep -q ":$p "; then
    fail "port $p: already listening（ss -ltn | grep :$p 定位占用者）"
  else
    ok "port $p: free"
  fi
done

# ── 出网（安装与首启依赖：GitHub releases / Docker Hub）────────
if curl -fsS --max-time 10 -o /dev/null https://github.com/fleetlyrun/fleetly/releases 2>/dev/null; then
  ok "egress: github.com reachable"
else
  fail "egress: github.com unreachable（release 产物下载依赖）"
fi
if curl -fsS --max-time 10 -o /dev/null https://registry-1.docker.io/v2/ 2>/dev/null; then
  ok "egress: docker hub registry reachable"
else
  warn "egress: docker hub registry probe 未确认（401 属正常可达信号；仅网络不可达才算 FAIL）"
fi

# ── 可选：DNS 对照（LE HTTP-01 前置）─────────────────────────
if [ $# -ge 1 ]; then
  domain=$1
  want_ip=$(curl -fsS --max-time 10 https://ifconfig.me 2>/dev/null || true)
  got_ip=$(getent ahostsv4 "$domain" 2>/dev/null | awk 'NR==1{print $1}')
  if [ -n "$got_ip" ] && [ "$got_ip" = "$want_ip" ]; then
    ok "DNS: $domain -> $got_ip (== this host)"
  elif [ -n "$got_ip" ]; then
    fail "DNS: $domain -> $got_ip (this host: ${want_ip:-unknown})——记录未指向本机"
  else
    fail "DNS: $domain does not resolve（LE 生产证书签发被阻断——先建 A 记录）"
  fi
fi

# ── 汇总 ────────────────────────────────────────────────────
printf '\n'
printf 'verify-vps: %d failure(s), %d warning(s)\n' "$failures" "$warns"
[ "$failures" -eq 0 ] || exit 1
exit 0
