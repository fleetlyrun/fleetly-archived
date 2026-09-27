#!/bin/sh
# spike/t20/scripts/in-s4-ordering.sh — Spike ④ 内层观测（dind 内运行）。
#
# 前置（宿主编排已就绪）：fleetlyd 已启动（127.0.0.1:8420/8421），swarm active，
# 二进制在 /opt/fleetly/bin，fixture 在 /opt/fleetly/，env：FLEETLY_TOKEN /
# FLEETLY_PROJECT / FLEETLY_ADDR。
#
# 产出：/tmp/s4-report.txt（人读汇总）、/tmp/s4-events-*.log（docker events 原文）、
# /tmp/s4-*-snapshots.txt（1s 粒度 service ps 快照）、/tmp/s4-deploy-*.log。
set -u

FCLI=/opt/fleetly/bin/fleetly
REPORT=/tmp/s4-report.txt
: >"$REPORT"
say() { printf '%s\n' "$*" | tee -a "$REPORT"; }

# ── S4.0 depends_on 现行口径（本地 validate，无副作用）
say '=== S4.0 depends_on validate (current stance) ==='
if $FCLI validate /opt/fleetly/ord3-depends-on.yaml >/tmp/s4-validate.txt 2>&1; then
    say 'validate-rc=0'
else
    say "validate-rc=$?"
fi
sed -n '1,6p' /tmp/s4-validate.txt >>"$REPORT"

# ── 通用观测原语
snapshot() { # <app> <services...>
    app=$1
    shift
    ts=$(date -u +%H:%M:%S)
    {
        echo "--- t=$ts ---"
        for s in "$@"; do
            printf '%s: ' "$s"
            docker service ps "fleetly-founder-default-$app-$s" --format '{{.CurrentState}}' 2>&1 | tr '\n' ';'
            echo
        done
    } >>"/tmp/s4-$app-snapshots.txt"
}

set +e
# ══ 场景 1：依赖链（base→mid→leaf；mid/leaf 依赖未就绪即退出）
say '=== S4.1 ord1: dependency chain (parallel create + self-heal) ==='
EV=/tmp/s4-events-ord1.log
docker events --format '{{.TimeNano}}|{{.Type}}|{{.Action}}|{{.Actor.ID}}|{{.Actor.Attributes.name}}' >"$EV" 2>&1 &
EVPID=$!
sleep 1
: >/tmp/s4-ord1-snapshots.txt
DEPLOY_START_S=$(date +%s)
timeout 600 $FCLI deploy /opt/fleetly/ord1-chain.yaml >/tmp/s4-deploy-ord1.log 2>&1 &
DEPLOY_PID=$!
while kill -0 "$DEPLOY_PID" >/dev/null 2>&1; do
    snapshot ord1 base mid leaf
    sleep 1
done
wait "$DEPLOY_PID"
DEPLOY_RC=$?
DEPLOY_END_S=$(date +%s)
say "ord1 deploy-rc=$DEPLOY_RC rc-wall-s=$((DEPLOY_END_S - DEPLOY_START_S))"
sleep 2
snapshot ord1 base mid leaf
kill "$EVPID" >/dev/null 2>&1 || true

$FCLI deployments list --json ord1 >/tmp/s4-ord1-deployments.json 2>&1
say "ord1 deployment: $(head -c 600 /tmp/s4-ord1-deployments.json)"
for s in base mid leaf; do
    say "ord1 service ps $s:"
    docker service ps "fleetly-founder-default-ord1-$s" --format '{{.Name}} {{.CurrentState}} {{.Error}}' >>"$REPORT" 2>&1
done
awk -F'|' -v tag='ord1' '
{
  ts=$1; type=$2; action=$3; name=$5; pfx="fleetly-founder-default-ord1-"
  if (type=="service" && action=="create" && index(name,pfx)==1) {
      svc=substr(name,length(pfx)+1)
      if (!(svc in screate)) { screate[svc]=ts; order[++n]=svc }
  }
  if (type=="container" && index(name,pfx)==1) {
      rest=substr(name,length(pfx)+1); split(rest,parts,"."); svc=parts[1]
      if (action=="start") { starts[svc]++; if (!(svc in fstart)) fstart[svc]=ts }
      if (action=="health_status: healthy") {
          if (!(svc in fhealthy)) { fhealthy[svc]=ts; starts_at_healthy[svc]=starts[svc] }
      }
      if (action=="die") dies[svc]++
  }
}
END {
  base=0
  for (k=1;k<=n;k++) { s=order[k]; if (base==0 || screate[s]<base) base=screate[s] }
  printf "ORD1 service order (create event, delta from earliest)\n"
  for (k=1;k<=n;k++) { s=order[k]; printf "  %s create+%.0fms\n", s, (screate[s]-base)/1000000.0 }
  for (k=1;k<=n;k++) {
      s=order[k]
      if (fstart[s]!="") printf "  %s create_to_start=%.0fms\n", s, (fstart[s]-screate[s])/1000000.0
      if (fhealthy[s]!="") printf "  %s create_to_healthy=%.0fms\n", s, (fhealthy[s]-screate[s])/1000000.0
      printf "  %s container_starts=%d (starts-until-first-healthy=%d) container_dies=%d\n", s, starts[s], starts_at_healthy[s]+0, dies[s]+0
  }
}
' "$EV" >>"$REPORT" 2>&1

# ══ 场景 2：永久失败服务（exit 1 重启循环）→ 看门狗判定与失败形态
say '=== S4.2 ord2: permanently failing service (restart loop) ==='
EV2=/tmp/s4-events-ord2.log
docker events --format '{{.TimeNano}}|{{.Type}}|{{.Action}}|{{.Actor.ID}}|{{.Actor.Attributes.name}}' >"$EV2" 2>&1 &
EVPID2=$!
sleep 1
DEPLOY_START_S=$(date +%s)
timeout 300 $FCLI deploy /opt/fleetly/ord2-broken.yaml >/tmp/s4-deploy-ord2.log 2>&1 &
DEPLOY_PID2=$!
: >/tmp/s4-ord2-snapshots.txt
while kill -0 "$DEPLOY_PID2" >/dev/null 2>&1; do
    snapshot ord2 good bad
    sleep 1
done
wait "$DEPLOY_PID2"
DEPLOY_RC2=$?
DEPLOY_END_S=$(date +%s)
say "ord2 deploy-rc=$DEPLOY_RC2 rc-wall-s=$((DEPLOY_END_S - DEPLOY_START_S))"
tail -5 /tmp/s4-deploy-ord2.log >>"$REPORT"
sleep 2
snapshot ord2 good bad
kill "$EVPID2" >/dev/null 2>&1 || true
$FCLI deployments list --json ord2 >/tmp/s4-ord2-deployments.json 2>&1
say "ord2 deployment: $(head -c 600 /tmp/s4-ord2-deployments.json)"
say 'ord2 service ls:'
docker service ls --format '{{.Name}} {{.Replicas}}' | grep 'ord2' >>"$REPORT" 2>&1
say 'ord2 bad service ps (restart loop):'
docker service ps fleetly-founder-default-ord2-bad --format '{{.Name}} {{.CurrentState}} {{.Error}}' | head -8 >>"$REPORT" 2>&1
awk -F'|' '
{
  ts=$1; type=$2; action=$3; name=$5; pfx="fleetly-founder-default-ord2-"
  if (type=="container" && index(name,pfx)==1) {
      rest=substr(name,length(pfx)+1); split(rest,parts,"."); svc=parts[1]
      if (action=="start") starts[svc]++
      if (action=="die") dies[svc]++
  }
}
END {
  printf "ORD2 container starts/dies: good=%d/%d bad=%d/%d\n", starts["good"]+0, dies["good"]+0, starts["bad"]+0, dies["bad"]+0
}
' "$EV2" >>"$REPORT" 2>&1

# 收尾：列出两个 app 的事件（便于报告引用）
say '=== S4.3 event digest (deployment.*) ==='
$FCLI events watch --since-seq 0 >/tmp/s4-event-watch.txt 2>&1 &
WPID=$!
sleep 3
kill "$WPID" >/dev/null 2>&1 || true
grep -E 'deployment\.|release\.' /tmp/s4-event-watch.txt | head -25 >>"$REPORT" 2>&1
say 'INNER_DONE'
