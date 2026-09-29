#!/bin/sh
# Phase D-1: 托管库创建 + 首部署(预期 fail-closed,创建 app 后提前收口)
set -eu
. /tmp/rb/env.sh

echo "=== 1. create managed redis (mlredis / redis-7) ==="
fleetly databases create --template redis-7 --json mlredis | grep -m 1 -o '"name":"[^"]*\|"status":"[^"]*' || fleetly databases create --template redis-7 mlredis
i=0
while [ $i -lt 60 ]; do
  st=$(fleetly databases get --json mlredis 2>/dev/null | grep -m 1 -o '"status":"[a-z]*' | cut -d'"' -f4)
  [ "$st" = "ready" ] && { echo "mlredis ready after ~$((i*5))s"; break; }
  sleep 5; i=$((i+1))
done
[ "$st" = "ready" ] || { echo "FATAL: mlredis not ready (state=$st)"; fleetly databases get mlredis; exit 1; }

echo "=== 2. reveal password to file (not printed) ==="
fleetly databases reveal --json mlredis > /tmp/rb/reveal.json
PW=$(grep -m 1 -o '"password":"[^"]*' /tmp/rb/reveal.json | cut -d'"' -f4)
[ -n "$PW" ] || { echo "FATAL: no password in reveal"; exit 1; }
umask 077
printf 'export MLREDIS_PW=%s\n' "$PW" > /tmp/rb/mlredis.sh
rm -f /tmp/rb/reveal.json
echo "password stored to /tmp/rb/mlredis.sh (len ${#PW})"

echo "=== 3. validate compose variant ==="
fleetly validate /tmp/rb/compose.yml || true

echo "=== 4. first deploy in background (expected fail-closed) ==="
nohup sh -c '. /tmp/rb/env.sh; fleetly deploy /tmp/rb/compose.yml' > /tmp/rb/deploy1.log 2>&1 &
echo "deploy pid $!"
i=0
while [ $i -lt 36 ]; do
  if fleetly apps get messageloop >/dev/null 2>&1; then echo "app created after ~$((i*5))s"; break; fi
  sleep 5; i=$((i+1))
done
fleetly apps get messageloop 2>&1 | grep -m 6 .

echo "=== 5. observe fail-closed reason then cancel early ==="
sleep 20
fleetly deployments list messageloop 2>&1 | grep -m 4 .
fleetly logs history messageloop --service messageloop --limit 6 2>&1 | grep -m 6 . || true
fleetly logs history messageloop --service mlbridge --limit 6 2>&1 | grep -m 6 . || true
DEP=$(fleetly deployments list --json messageloop 2>/dev/null | grep -m 1 -o '"id":"[^"]*' | cut -d'"' -f4)
echo "deployment id: $DEP"
if [ -n "${DEP:-}" ]; then fleetly deployments cancel "$DEP" && echo "cancelled early (scale=0 preserved)"; fi
sleep 5
fleetly apps get messageloop 2>&1 | grep -m 8 .
echo "PHASE_D1_DONE"
