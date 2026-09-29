#!/bin/sh
# Phase C: bootstrap(注册首用户=平台管理员→铸 machine 令牌→CLI 就绪)
set -eu
GW=https://127.0.0.1:8420
JAR=/tmp/rb/session.jar

echo "=== 1. register founder (first user = platform admin) ==="
RC=$(curl -sk -c $JAR -o /tmp/rb/reg.json -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"email":"founder@fleetly.run","password":"founder-pass-1","display_name":"Founder"}' \
  $GW/v1/auth/register)
echo "register http:$RC"
[ "$RC" = "200" ] || { cat /tmp/rb/reg.json; echo; exit 1; }

echo "=== 2. bootstrap token must now be revoked ==="
BT=$(cat /var/lib/fleetly/bootstrap-token 2>/dev/null || true)
if [ -n "$BT" ]; then
  code=$(curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $BT" $GW/v1/apps)
  echo "bootstrap GET /v1/apps -> $code (expect 401)"
fi

echo "=== 3. mint machine token via founder session ==="
RC=$(curl -sk -b $JAR -o /tmp/rb/mtok.json -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"machine":true,"note":"staging-rebuild-deploy","scopes":["admin"]}' \
  $GW/v1/tokens)
echo "mint http:$RC"
[ "$RC" = "200" ] || { cat /tmp/rb/mtok.json; echo; exit 1; }
MT=$(grep -o '"token":"[^"]*' /tmp/rb/mtok.json | cut -d'"' -f4)
[ -n "$MT" ] || { echo "FATAL: no token in mint response"; exit 1; }
umask 077
printf 'export FLEETLY_TOKEN=%s\nexport FLEETLY_ADDR=127.0.0.1:8421\nexport FLEETLY_TLS=insecure\nexport FLEETLY_PROJECT=founder/default\n' "$MT" > /tmp/rb/env.sh
rm -f $JAR /tmp/rb/mtok.json /tmp/rb/reg.json
echo "machine token stored to /tmp/rb/env.sh ($(wc -c < /tmp/rb/env.sh) bytes, not printed)"

echo "=== 4. CLI login + sanity ==="
. /tmp/rb/env.sh
fleetly auth login --token "$FLEETLY_TOKEN" >/dev/null && echo "cli login ok"
echo "--- auth status ---"
fleetly auth status 2>&1 | grep -m 6 .
echo "--- apps list ---"
fleetly apps list 2>&1 | grep -m 4 .
echo "PHASE_C_DONE"
