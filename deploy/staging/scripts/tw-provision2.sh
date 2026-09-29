#!/bin/sh
# 供给续段 v2:sign-in(cookie)→API key→runbook up→attach→换接→redeploy
set -eu
. /tmp/rb/env.sh
. /tmp/rb/secrets.sh
GW=https://tw-app.dev.fleetly.run
JAR=/tmp/rb/twjar.txt

echo "=== 1. sign-in admin (cookie jar) ==="
RC=$(curl -s -c $JAR -o /tmp/rb/signin.json -w '%{http_code}' -H 'Content-Type: application/json' \
  -d "{\"email\":\"founder@torchwood.local\",\"password\":\"$TW_ADMIN_PW\"}" \
  $GW/v1/console/auth/sign-in)
echo "sign-in http:$RC"
[ "$RC" = "200" ] || { cat /tmp/rb/signin.json; echo; exit 1; }
rm -f /tmp/rb/signin.json
grep -q TORCHWOOD_session_console $JAR && echo "console session cookie captured"

echo "=== 2. create mlbridge API key (cookie auth) ==="
RC=$(curl -s -b $JAR -o /tmp/rb/key.json -w '%{http_code}' -H 'Content-Type: application/json' \
  -H 'X-Torchwood-Project: staging' \
  -d '{"name":"mlbridge-staging","scopes":["users.read","databases.read","databases.write","runbooks.read","runbooks.write","analytics.write"]}' \
  $GW/v1/server/api-keys)
echo "api-key http:$RC"
[ "$RC" = "200" ] || { cat /tmp/rb/key.json; echo; exit 1; }
KEY=$(grep -o '"secret":"[^"]*' /tmp/rb/key.json | cut -d'"' -f4)
[ -n "$KEY" ] || { echo "FATAL no secret"; cat /tmp/rb/key.json; exit 1; }
grep -q TW_ML_KEY /tmp/rb/secrets.sh || printf 'export TW_ML_KEY=%s\n' "$KEY" >> /tmp/rb/secrets.sh
rm -f $JAR /tmp/rb/key.json
echo "key stored (len ${#KEY})"

echo "=== 3. apply mlbridge runbooks ==="
/tmp/rb/torchwood-cli runbook up --dir /tmp/rb/mlrunbooks --api-key "$KEY" --endpoint tw-grpc.dev.fleetly.run:443 --tls 2>&1 | tail -n 8

echo "=== 4. project network attach (both apps) ==="
fleetly projects network attach torchwood 2>&1 | grep -m 2 .
fleetly projects network attach messageloop 2>&1 | grep -m 2 .

echo "=== 5. redeploy torchwood (project net roll) ==="
fleetly deploy /tmp/rb/torchwood-compose.yml 2>&1 | tail -n 4

echo "=== 6. mlbridge rewire to staging torchwood ==="
fleetly env set messageloop MLBRIDGE_TORCHWOOD_BASE_URL "http://torchwood-server:9080" >/dev/null && echo ok-base-url
fleetly env set messageloop MLBRIDGE_TORCHWOOD_PROJECTS "[{\"project_id\":\"staging\",\"api_key\":\"$KEY\"}]" >/dev/null && echo ok-projects

echo "=== 7. redeploy messageloop ==="
fleetly deploy /tmp/rb/compose.yml 2>&1 | tail -n 4

echo "PROVISION2_DONE"
