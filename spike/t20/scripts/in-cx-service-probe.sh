#!/bin/sh
# spike/t20/scripts/in-cx-service-probe.sh — 跨节点（swarm service 任务）探针。
#
# 用法（服务任务内）：sh in-cx-service-probe.sh <service-name>
# 等待 tasks.<service> 双任务 DNS 注册（跨节点任务的 DNS 面），逐地址 ICMP；
# 再做公网面判定。等待窗上限 ~60s（DNS 注册时序与任务调度时序解耦）。
set -u

svc=${1:?usage: in-cx-service-probe.sh <service-name>}
say() { printf '%s\n' "$*"; }

say "SVC_PROBE_BEGIN host=$(hostname) my_ip=$(hostname -i 2>&1)"
say "ROUTE_TABLE=$(sed -n '2,$p' /proc/net/route | awk '{print $2"->"$3}' | tr '\n' ',' | sed 's/,$//')"
say "INTERFACES=$(ls /sys/class/net 2>&1 | tr '\n' ',' | sed 's/,$//')"

# 任务 DNS 注册有调度时序：轮询直到出现 >=2 个任务地址或超时（~60s）。
attempt=0
count=0
while [ "$attempt" -lt 20 ]; do
    nslookup "tasks.$svc" >/tmp/t20-cx-lookup.txt 2>&1 || true
    count=$(grep '^Address' /tmp/t20-cx-lookup.txt | awk '{print $2}' | grep -v '^127\.' | grep -v ':' | sort -u | wc -l | tr -d ' ')
    [ "$count" -ge 2 ] && break
    attempt=$((attempt + 1))
    sleep 3
done
if [ "$count" -ge 2 ]; then
    say "SVC_TASKS_DNS=ok"
else
    say "SVC_TASKS_DNS=fail"
    say "SVC_TASKS_DNS_OUT=$(sed -n '1,6p' /tmp/t20-cx-lookup.txt | tr '\n' ';')"
fi
addresses=$(grep '^Address' /tmp/t20-cx-lookup.txt | awk '{print $2}' | grep -v '^127\.' | grep -v ':' | sort -u)
n=0
ok=0
for addr in $addresses; do
    n=$((n + 1))
    if ping -c 2 -W 2 "$addr" >/tmp/t20-cx-ping.txt 2>&1; then
        say "SVC_PING_$n=$addr ok"
        ok=$((ok + 1))
    else
        say "SVC_PING_$n=$addr fail"
    fi
done
say "SVC_TASK_ADDRESSES=$n (ping-ok=$ok)"

if nslookup example.com >/tmp/t20-cx-dnspub.txt 2>&1; then
    say "DNS_PUBLIC=ok"
else
    say "DNS_PUBLIC=fail"
    say "DNS_PUBLIC_OUT=$(head -1 /tmp/t20-cx-dnspub.txt)"
fi
if nc -w 3 1.1.1.1 80 </dev/null >/tmp/t20-cx-nc.txt 2>&1; then
    say "TCP_PUBLIC_80=ok"
else
    say "TCP_PUBLIC_80=fail"
    say "TCP_PUBLIC_80_OUT=$(head -1 /tmp/t20-cx-nc.txt)"
fi
say "SVC_PROBE_END"
