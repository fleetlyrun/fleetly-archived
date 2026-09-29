#!/bin/sh
# torchwood 两阶段部署(staging 全托管库变体)
set -eu
. /tmp/rb/env.sh
. /tmp/rb/secrets.sh

echo "=== 0. pre-pull third-party images (by tag) ==="
docker pull -q migrate/migrate:v4.18.1 >/dev/null && echo "migrate ok" || echo "migrate PULL FAIL"
docker pull -q alpine/socat:1.8.0.1 >/dev/null && echo "socat ok" || echo "socat PULL FAIL"
docker pull -q pgsty/silo:RELEASE.2026-09-03T13-18-01Z >/dev/null && echo "silo ok" || echo "silo PULL FAIL"
docker pull -q ghcr.io/torchwoodcloud/torchwood:sha-78ea1a4 >/dev/null && echo "torchwood ok" || echo "torchwood PULL FAIL"

echo "=== 1. first deploy (expected E_CONFIG_NOT_FOUND; creates app) ==="
fleetly deploy /tmp/rb/torchwood-compose.yml > /tmp/rb/twd1.log 2>&1 || true
tail -n 3 /tmp/rb/twd1.log | cut -c 1-200
fleetly apps get torchwood >/dev/null 2>&1 && echo "app torchwood created"

echo "=== 2. env set (secrets generated in place; values not printed) ==="
umask 077
TW_AUTH_PW=$(openssl rand -hex 24)
JWT=$(openssl rand -hex 32)
SETUP=$(openssl rand -hex 32)
MINIO_U=$(openssl rand -hex 12)
MINIO_P=$(openssl rand -hex 32)
DISP_TOK=$(openssl rand -hex 32)
PACK_TOK=$(openssl rand -hex 32)
printf 'export TW_SETUP_TOKEN=%s\n' "$SETUP" >> /tmp/rb/secrets.sh
fleetly env set torchwood TORCHWOOD_SECURITY_JWT_SECRET "$JWT" >/dev/null && echo ok-jwt
fleetly env set torchwood TORCHWOOD_SECURITY_SETUP_TOKEN "$SETUP" >/dev/null && echo ok-setup
fleetly env set torchwood TORCHWOOD_SERVER_HTTP_PUBLIC_URL "https://tw-app.dev.fleetly.run" >/dev/null && echo ok-public-url
fleetly env set torchwood TORCHWOOD_AUTH_PASSWORD "$TW_AUTH_PW" >/dev/null && echo ok-auth-pw
fleetly env set torchwood TORCHWOOD_DATA_DATABASE_SOURCE "postgres://tw_authenticator:${TW_AUTH_PW}@torchwood-pg:5432/torchwood_pg?sslmode=disable" >/dev/null && echo ok-dsn
fleetly env set torchwood MINIO_ROOT_USER "$MINIO_U" >/dev/null && echo ok-minio-user
fleetly env set torchwood MINIO_ROOT_PASSWORD "$MINIO_P" >/dev/null && echo ok-minio-pass
fleetly env set torchwood TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID "$MINIO_U" >/dev/null && echo ok-s3-id
fleetly env set torchwood TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY "$MINIO_P" >/dev/null && echo ok-s3-secret
fleetly env set torchwood TORCHWOOD_FUNCTIONS_DISPATCHER_SHARED_TOKEN "$DISP_TOK" >/dev/null && echo ok-dispatcher-token
fleetly env set torchwood TORCHWOOD_FUNCTIONS_PACKER_SHARED_TOKEN "$PACK_TOK" >/dev/null && echo ok-packer-token
fleetly env set torchwood TORCHWOOD_DATA_REDIS_ADDR "twredis:6379" >/dev/null && echo ok-redis-addr
fleetly env set torchwood TORCHWOOD_DATA_REDIS_PASSWORD "$TRPW" >/dev/null && echo ok-redis-pass

echo "--- machine token (scopes tasks,build) for dispatcher ---"
fleetly tokens create --machine --scopes tasks,build --note "torchwood dispatcher (fleetly Tasks/build)" --json > /tmp/rb/twmtok.json
MT=$(grep -m 1 '"token"' /tmp/rb/twmtok.json | cut -d'"' -f4)
[ -n "$MT" ] || { echo "FATAL: no machine token"; exit 1; }
fleetly env set torchwood TORCHWOOD_FUNCTIONS_FLEETLY_TOKEN "$MT" >/dev/null && echo ok-fleetly-token
rm -f /tmp/rb/twmtok.json

echo "=== 3. configs (short keys to dodge swarm 64-char limit) ==="
fleetly configs set --from-file /tmp/rb/tw/config.yaml torchwood config.yaml >/dev/null && echo ok-config-yaml
fleetly configs set --from-file /tmp/rb/tw/bootstrap-runtime.sql torchwood runtime.sql >/dev/null && echo ok-runtime-sql
fleetly configs set --from-file /tmp/rb/tw/bootstrap-roles.sql torchwood roles.sql >/dev/null && echo ok-roles-sql
fleetly configs ls torchwood 2>&1 | grep -m 5 .

echo "=== 4. domains ==="
fleetly domains add --service server --port 9080 --protocol http torchwood tw-app.dev.fleetly.run 2>&1 | grep -m 1 .
fleetly domains add --service server --port 9060 --protocol h2c torchwood tw-grpc.dev.fleetly.run 2>&1 | grep -m 1 .

echo "=== 5. second deploy (init jobs then resident services) ==="
fleetly deploy /tmp/rb/torchwood-compose.yml 2>&1 | tail -n 10

echo "=== 6. app state ==="
fleetly apps get torchwood 2>&1 | grep -m 8 .
echo "TORCHWOOD_DEPLOY_DONE"
