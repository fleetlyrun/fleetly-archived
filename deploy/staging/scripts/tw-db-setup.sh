#!/bin/sh
# torchwood 托管库创建(percona-18 + redis-7)
set -eu
. /tmp/rb/env.sh

echo "=== pre-pull percona template image (by tag) ==="
docker pull -q percona/percona-distribution-postgresql:18 >/dev/null && echo "percona ok" || echo "percona pull failed (continuing)"

echo "=== create torchwood-pg (percona-postgresql-18) ==="
fleetly databases create --template percona-postgresql-18 --json torchwood-pg > /tmp/rb/twpg-create.json 2>&1 || true
grep -m 1 '"name"' /tmp/rb/twpg-create.json | head -c 200 || cat /tmp/rb/twpg-create.json
i=0; st=provisioning
while [ $i -lt 60 ]; do
  st=$(fleetly databases get --json torchwood-pg 2>/dev/null | grep -m 1 '"status"' | cut -d'"' -f4)
  [ "$st" = "ready" ] && { echo "torchwood-pg ready after ~$((i*5))s"; break; }
  sleep 5; i=$((i+1))
done
[ "$st" = "ready" ] || { echo "FATAL: torchwood-pg state=$st"; fleetly databases get torchwood-pg; exit 1; }

echo "=== create twredis (redis-7) ==="
fleetly databases create --template redis-7 --json twredis > /tmp/rb/twredis-create.json 2>&1 || true
grep -m 1 '"name"' /tmp/rb/twredis-create.json | head -c 200 || cat /tmp/rb/twredis-create.json
i=0; st=provisioning
while [ $i -lt 60 ]; do
  st=$(fleetly databases get --json twredis 2>/dev/null | grep -m 1 '"status"' | cut -d'"' -f4)
  [ "$st" = "ready" ] && { echo "twredis ready after ~$((i*5))s"; break; }
  sleep 5; i=$((i+1))
done
[ "$st" = "ready" ] || { echo "FATAL: twredis state=$st"; exit 1; }

echo "=== reveal both (to files, not printed) ==="
fleetly databases reveal --json torchwood-pg > /tmp/rb/twpg-reveal.json
TWPW=$(grep -m 1 '"password"' /tmp/rb/twpg-reveal.json | cut -d'"' -f4)
fleetly databases reveal --json twredis > /tmp/rb/twredis-reveal.json
TRPW=$(grep -m 1 '"password"' /tmp/rb/twredis-reveal.json | cut -d'"' -f4)
[ -n "$TWPW" ] && [ -n "$TRPW" ] || { echo "FATAL: reveal missing"; exit 1; }
rm -f /tmp/rb/twpg-reveal.json /tmp/rb/twredis-reveal.json /tmp/rb/twpg-create.json /tmp/rb/twredis-create.json
umask 077
grep -v MLREDIS_PW /tmp/rb/mlredis.sh > /tmp/rb/secrets.sh 2>/dev/null || printf '' > /tmp/rb/secrets.sh
printf 'export TWPW=%s\nexport TRPW=%s\n' "$TWPW" "$TRPW" >> /tmp/rb/secrets.sh
echo "passwords stored to /tmp/rb/secrets.sh (pg len ${#TWPW}, redis len ${#TRPW})"

echo "=== db list ==="
fleetly databases list 2>&1 | grep -m 6 .
echo "TW_DB_DONE"
