set -u
SERVICE_TOKEN="${OPENRAILS_SERVICE_TOKEN:?set OPENRAILS_SERVICE_TOKEN (docker compose exec -T openrails /usr/local/bin/openrails --config /app/config/openrails.config.yaml mint-operator-service-token)}"
B="${BASE_URL:-http://127.0.0.1:22053}/v1/service"
AUTH="Authorization: Bearer $SERVICE_TOKEN"
CT="JT="; CT="Content-Type: application/json"
uuid(){ cat /proc/sys/kernel/random/uuid; }
pass=0; fail=0
ck(){ # desc expected_code actual_code [substr] [body]
  if [ "$2" = "$3" ]; then echo "PASS: $1 ($3)"; pass=$((pass+1)); else echo "FAIL: $1 want=$2 got=$3  body=$5"; fail=$((fail+1)); fi
}
code(){ curl -sS -m15 -o /tmp/b.json -w '%{http_code}' "$@"; }

OWNER1=$(uuid); OWNER2=$(uuid); OWNER3=$(uuid); CTYPE="e2e_admit_$(date +%s)"

# credit type
code -H "$AUTH" -H "$CT" -X POST "$B/credit-types" -d "{\"name\":\"$CTYPE\",\"display_name\":\"E2E\",\"unit\":\"USD\",\"decimal_places\":2}" >/dev/null

# OWNER1 tier: 2 req/60s, entitled gpt-4o, big budget; deposit
curl -sS -m15 -H "$AUTH" -H "$CT" -X PUT "$B/tier-policies" -d "{\"owner_id\":\"$OWNER1\",\"tier\":\"free\",\"windows\":[{\"unit\":\"request\",\"window_seconds\":60,\"max\":2}],\"entitled_endpoints\":[\"gpt-4o\"],\"budget_windows\":[{\"key\":\"1h\",\"window_seconds\":3600,\"limit_millicents\":1000000}]}" >/dev/null
curl -sS -m15 -H "$AUTH" -H "$CT" -X POST "$B/credits/deposit" -d "{\"owner_id\":\"$OWNER1\",\"user_id\":\"$OWNER1\",\"credit_type\":\"$CTYPE\",\"amount\":100000,\"source\":\"e2e\",\"source_id\":\"$(uuid)\"}" >/dev/null

adm(){ # owner model estimate -> sets C (code), reads /tmp/b.json
  C=$(code -H "$AUTH" -H "$CT" -X POST "$B/admit" -d "{\"owner_id\":\"$1\",\"invoker\":\"user:e2e\",\"tier\":\"free\",\"model\":\"$2\",\"amounts\":{\"request\":1},\"credit_type\":\"$CTYPE\",\"estimate_cents\":$3,\"request_id\":\"$(uuid)\"}")
}

echo "--- #298 throughput (max 2/min) ---"
adm "$OWNER1" gpt-4o 100; ck "admit 1 allowed" 200 "$C" "" "$(cat /tmp/b.json)"
adm "$OWNER1" gpt-4o 100; ck "admit 2 allowed" 200 "$C" "" "$(cat /tmp/b.json)"
# 3rd: throughput deny -> 429 + headers
hdr=$(curl -sS -m15 -D - -o /tmp/b.json -H "$AUTH" -H "$CT" -X POST "$B/admit" -d "{\"owner_id\":\"$OWNER1\",\"invoker\":\"user:e2e\",\"tier\":\"free\",\"model\":\"gpt-4o\",\"amounts\":{\"request\":1},\"credit_type\":\"$CTYPE\",\"estimate_cents\":100,\"request_id\":\"$(uuid)\"}" -w '%{http_code}')
c3=$(printf '%s' "$hdr" | tail -n1)
ck "admit 3 throughput 429" 429 "$c3" "" "$(cat /tmp/b.json)"
echo "$hdr" | grep -qi "X-RateLimit-Limit-request: 2" && { echo "PASS: X-RateLimit-Limit-request header"; pass=$((pass+1)); } || { echo "FAIL: missing X-RateLimit-Limit-request header"; fail=$((fail+1)); }
echo "$hdr" | grep -qi "Retry-After:" && { echo "PASS: Retry-After header"; pass=$((pass+1)); } || { echo "FAIL: missing Retry-After"; fail=$((fail+1)); }

echo "--- #298 endpoint gating ---"
adm "$OWNER1" dall-e-3 100; ck "non-entitled endpoint 403" 403 "$C" "" "$(cat /tmp/b.json)"
grep -q '"blocked_by":"endpoint"' /tmp/b.json && { echo "PASS: blocked_by endpoint"; pass=$((pass+1)); } || { echo "FAIL: blocked_by!=endpoint: $(cat /tmp/b.json)"; fail=$((fail+1)); }

echo "--- #304 budget deny ---"
curl -sS -m15 -H "$AUTH" -H "$CT" -X PUT "$B/tier-policies" -d "{\"owner_id\":\"$OWNER2\",\"tier\":\"free\",\"windows\":[{\"unit\":\"request\",\"window_seconds\":60,\"max\":1000}],\"budget_windows\":[{\"key\":\"1h\",\"window_seconds\":3600,\"limit_millicents\":500}]}" >/dev/null
curl -sS -m15 -H "$AUTH" -H "$CT" -X POST "$B/credits/deposit" -d "{\"owner_id\":\"$OWNER2\",\"user_id\":\"$OWNER2\",\"credit_type\":\"$CTYPE\",\"amount\":100000,\"source\":\"e2e\",\"source_id\":\"$(uuid)\"}" >/dev/null
adm "$OWNER2" gpt-4o 400; ck "budget under-limit allowed" 200 "$C" "" "$(cat /tmp/b.json)"
adm "$OWNER2" gpt-4o 200; ck "budget over-limit denied" 403 "$C" "" "$(cat /tmp/b.json)"
grep -q '"blocked_by":"budget"' /tmp/b.json && { echo "PASS: blocked_by budget"; pass=$((pass+1)); } || { echo "FAIL: blocked_by!=budget: $(cat /tmp/b.json)"; fail=$((fail+1)); }

echo "--- #298 money deny (no balance) ---"
curl -sS -m15 -H "$AUTH" -H "$CT" -X PUT "$B/tier-policies" -d "{\"owner_id\":\"$OWNER3\",\"tier\":\"free\",\"windows\":[{\"unit\":\"request\",\"window_seconds\":60,\"max\":1000}]}" >/dev/null
adm "$OWNER3" gpt-4o 500; ck "money deny 402" 402 "$C" "" "$(cat /tmp/b.json)"
grep -q '"blocked_by":"money"' /tmp/b.json && { echo "PASS: blocked_by money"; pass=$((pass+1)); } || { echo "FAIL: blocked_by!=money: $(cat /tmp/b.json)"; fail=$((fail+1)); }

echo "--- #304 budget introspection ---"
C=$(code -H "$AUTH" -H "$CT" "$B/budget?owner_id=$OWNER2&invoker=user:e2e&tier=free")
ck "GET /budget 200" 200 "$C" "" "$(cat /tmp/b.json)"
grep -q '"limit":500' /tmp/b.json && { echo "PASS: budget window limit present"; pass=$((pass+1)); } || { echo "FAIL: budget windows: $(cat /tmp/b.json)"; fail=$((fail+1)); }

echo "--- #299 unverified arrears deny ---"
OWNER4=$(uuid)
curl -sS -m15 -H "$AUTH" -H "$CT" -X PUT "$B/tier-policies" -d "{\"owner_id\":\"$OWNER4\",\"tier\":\"free\",\"windows\":[{\"unit\":\"request\",\"window_seconds\":60,\"max\":1000}]}" >/dev/null
curl -sS -m15 -H "$AUTH" -H "$CT" -X PUT "$B/credits/account-settings" -d "{\"payer\":\"$OWNER4\",\"credit_type\":\"$CTYPE\",\"billing_mode\":\"arrears\"}" >/dev/null
adm "$OWNER4" gpt-4o 100; ck "unverified arrears 403" 403 "$C" "" "$(cat /tmp/b.json)"
grep -q '"blocked_by":"unverified"' /tmp/b.json && { echo "PASS: blocked_by unverified"; pass=$((pass+1)); } || { echo "FAIL: unverified: $(cat /tmp/b.json)"; fail=$((fail+1)); }

echo "==== RESULT: pass=$pass fail=$fail ===="
