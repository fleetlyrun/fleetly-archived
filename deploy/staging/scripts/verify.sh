#!/bin/sh
# Phase E: 端到端验证
set -u
. /tmp/rb/env.sh
. /tmp/rb/mlredis.sh

echo "=== 1. app/services/tasks ==="
fleetly apps get messageloop 2>&1 | grep -m 12 .
docker service ls --format '{{.Name}} {{.Replicas}}' | grep messageloop
docker ps --format '{{.Names}}\t{{.Status}}' | grep -E 'messageloop|mlbridge' | cut -c 1-120

echo "=== 2. service logs (startup errors?) ==="
echo "--- messageloop ---"
fleetly logs history --service messageloop --limit 8 messageloop 2>&1 | grep -m 8 .
echo "--- mlbridge ---"
fleetly logs history --service mlbridge --limit 8 messageloop 2>&1 | grep -m 8 .

echo "=== 3. managed redis probe (via db shared net, alias mlredis) ==="
NET=$(docker network ls --format '{{.Name}}' | grep 'mlredis-net')
echo "net: $NET"
docker run --rm --network "$NET" -e PW="$MLREDIS_PW" redis:7 sh -c 'redis-cli --no-auth-warning -a "$PW" -h mlredis ping && redis-cli --no-auth-warning -a "$PW" -h mlredis info server | grep -m 2 redis_version'

echo "=== 4. db env materialized into tasks? ==="
MLC=$(docker ps --format '{{.Names}}' | grep 'messageloop-messageloop' | grep -v mlbridge | grep -m 1 .)
echo "task: $MLC"
docker exec "$MLC" sh -c 'env | grep -c FLEETLY_DB_MLREDIS && env | grep FLEETLY_DB_MLREDIS_HOST'

echo "=== 5. domains ==="
fleetly domains verify messageloop 2>&1 | grep -m 15 .

echo "=== 6. edge probes ==="
curl -sk -o /dev/null -w 'ml-ws /ws -> %{http_code}\n' https://ml-ws.dev.fleetly.run/ws
curl -sk -o /dev/null -w 'ml-api / -> %{http_code}\n' https://ml-api.dev.fleetly.run/
echo | openssl s_client -connect 146.190.58.0:443 -servername ml-grpc.dev.fleetly.run -alpn h2 2>/dev/null | grep -E 'ALPN protocol|Verify return code'

echo "=== 7. VL ingestion (logs history already reads VL; search face) ==="
fleetly logs search --keyword redis messageloop 2>&1 | grep -m 5 . || true

echo "PHASE_E_DONE"
