#!/bin/sh
# spike/t20/scripts/in-overlay-probe.sh — 容器内出网/DNS/对等面探针。
#
# 用法（容器内）：sh in-overlay-probe.sh <peer-name>
# 逐项打印 KEY=VALUE 行；全部有界超时（无网络时最坏 ~20s 退出）。
# 依赖 alpine busybox：nslookup / ping / nc / wget。
set -u

peer=${1:?usage: in-overlay-probe.sh <peer-name>}

say() { printf '%s\n' "$*"; }
firstline() { head -1 "$1" 2>&1 | tr -d '\r'; }

say "PROBE_BEGIN host=$(hostname)"
# 路由表（/proc/net/route 第 2 列是目的网络，十六进制小端；00000000=default）。
say "ROUTE_TABLE=$(sed -n '2,$p' /proc/net/route | awk '{print $2"->"$3}' | tr '\n' ',' | sed 's/,$//')"
say "NAMESERVERS=$(grep '^nameserver' /etc/resolv.conf | awk '{print $2}' | tr '\n' ',' | sed 's/,$//')"
say "INTERFACES=$(ls /sys/class/net 2>&1 | tr '\n' ',' | sed 's/,$//')"

# ── 对等面（同网络内另一容器）
if nslookup "$peer" >/tmp/t20-dns-peer.txt 2>&1; then
    say "DNS_PEER=ok"
    say "DNS_PEER_ADDR=$(grep -A2 'Name:' /tmp/t20-dns-peer.txt | grep '^Address' | head -1 | awk '{print $2}')"
else
    say "DNS_PEER=fail"
    say "DNS_PEER_OUT=$(firstline /tmp/t20-dns-peer.txt)"
fi
if ping -c 2 -W 2 "$peer" >/tmp/t20-ping-peer.txt 2>&1; then
    say "PING_PEER=ok"
else
    say "PING_PEER=fail"
fi
say "PING_PEER_SUMMARY=$(grep -E 'packets transmitted|packet loss' /tmp/t20-ping-peer.txt | tr '\n' ';' | sed 's/;*$//')"
if nc -w 3 "$peer" 8080 </dev/null >/tmp/t20-nc-peer.txt 2>&1; then
    say "TCP_PEER=ok"
else
    say "TCP_PEER=fail"
    say "TCP_PEER_OUT=$(firstline /tmp/t20-nc-peer.txt)"
fi

# ── 公网面（DNS / TCP×2 / HTTP）
if nslookup example.com >/tmp/t20-dns-pub.txt 2>&1; then
    say "DNS_PUBLIC=ok"
else
    say "DNS_PUBLIC=fail"
    say "DNS_PUBLIC_OUT=$(firstline /tmp/t20-dns-pub.txt)"
fi
for tcp in 80 53; do
    case $tcp in
    80) target=1.1.1.1 ;;
    53) target=8.8.8.8 ;;
    esac
    if nc -w 3 "$target" "$tcp" </dev/null >/tmp/t20-nc.txt 2>&1; then
        say "TCP_PUBLIC_$tcp=ok"
    else
        say "TCP_PUBLIC_$tcp=fail"
        say "TCP_PUBLIC_${tcp}_OUT=$(firstline /tmp/t20-nc.txt)"
    fi
done
if wget -T 5 -t 1 -q -O /dev/null http://1.1.1.1/ >/tmp/t20-wget.txt 2>&1; then
    say "HTTP_PUBLIC=ok"
else
    say "HTTP_PUBLIC=fail"
    say "HTTP_PUBLIC_OUT=$(firstline /tmp/t20-wget.txt)"
fi
say "PROBE_END"
