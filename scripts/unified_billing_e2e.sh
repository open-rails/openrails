#!/bin/sh
# Unified-billing e2e harness — deployed-stack edition (issue #244).
#
# Drives the FULL unified-billing money path against a *running, standalone*
# OpenRails over its service token-authenticated public service routes — the exact
# server-to-server contract gen-orchestrator / Tensorhub use in production
# (issue #233 topology, #222 public service token routes). Unlike the in-repo Go harness
# (tests/unified_billing_e2e_test.go, which needs testcontainers), this hits a
# real deployed service + its own Postgres, so it runs anywhere the stack is up
# (e.g. ~/cozy/e2e) and is the artifact that proved #244 end-to-end.
#
# Covers: create credit-type -> deposit -> GET balance (#247) -> atomic
# authorize+hold (#235) -> partial capture -> balance reflects actual ->
# over-balance authorize DENIED (prepaid gate) -> set arrears + cap (#242) ->
# read settings back. A FRESH credit type per run makes balances deterministic.
#
# POSIX sh + curl only (runs in alpine/curl). Usage:
#   OPENRAILS_SERVICE_TOKEN=openrails_st_xxx BASE_URL=http://openrails:2053 \
#   USER_ID=66666666-6666-6666-6666-666666666666 \
#     sh scripts/unified_billing_e2e.sh
#
# In ~/cozy/e2e: mint the service token with
#   docker compose exec -T openrails /usr/local/bin/openrails \
#     --config /app/config/openrails.config.yaml mint-operator-service-token
# then run this from a container ON the e2e_default network (openrails:2053 is
# not host-published):
#   docker run --rm --network e2e_default \
#     -v "$PWD/scripts/unified_billing_e2e.sh:/h.sh:ro" \
#     -e OPENRAILS_SERVICE_TOKEN=... -e BASE_URL=http://openrails:2053 \
#     --entrypoint sh alpine/curl:latest /h.sh
set -u

BASE_URL="${BASE_URL:-http://127.0.0.1:2053}"
: "${OPENRAILS_SERVICE_TOKEN:?set OPENRAILS_SERVICE_TOKEN to a minted operator service token}"
USER_ID="${USER_ID:-66666666-6666-6666-6666-666666666666}"
SVC="$BASE_URL/v1/service"
CREDITS="$SVC/credits"

uuid() { cat /proc/sys/kernel/random/uuid 2>/dev/null || echo "00000000-0000-4000-8000-$(date +%N)0000"; }
# A fresh credit type per run -> (USER_ID, type) balance starts at 0, so every
# absolute assertion below is deterministic regardless of prior runs.
CREDIT_TYPE="${CREDIT_TYPE:-e2e_$(uuid | tr -d -)}"

pass=0
fail=0
CODE=""
BODY=""

req() { # METHOD PATH [JSON] -> $CODE, $BODY
  _m="$1"; _p="$2"; _d="${3:-}"
  if [ -n "$_d" ]; then
    _out=$(curl -sS -m 20 -w '\n%{http_code}' \
      -H "Authorization: Bearer $OPENRAILS_SERVICE_TOKEN" -H "Content-Type: application/json" \
      -X "$_m" "$_p" -d "$_d")
  else
    _out=$(curl -sS -m 20 -w '\n%{http_code}' \
      -H "Authorization: Bearer $OPENRAILS_SERVICE_TOKEN" -H "Content-Type: application/json" \
      -X "$_m" "$_p")
  fi
  CODE=$(printf '%s' "$_out" | tail -n1)
  BODY=$(printf '%s' "$_out" | sed '$d')
}

expect() { # DESC WANT_CODE [SUBSTRING_REQUIRED]
  _desc="$1"; _want="$2"; _needle="${3:-}"
  if [ "$CODE" = "$_want" ] && { [ -z "$_needle" ] || printf '%s' "$BODY" | grep -qF "$_needle"; }; then
    echo "  PASS  $_desc  [HTTP $CODE]"; pass=$((pass+1))
  else
    echo "  FAIL  $_desc  [HTTP $CODE want $_want]${_needle:+ expected: $_needle}"
    echo "        body: $BODY"; fail=$((fail+1))
  fi
}

jget() { printf '%s' "$BODY" | sed -n 's/.*"'"$1"'":[[:space:]]*"\{0,1\}\([^",}]*\).*/\1/p' | head -n1; }

echo "== unified-billing e2e against $BASE_URL (credit_type=$CREDIT_TYPE) =="

# 0. Create the credit type (#247 catalog surface).
req POST "$SVC/credit-types" "{\"name\":\"$CREDIT_TYPE\",\"display_name\":\"E2E Credits\",\"unit\":\"USD\",\"decimal_places\":2}"
expect "create credit-type" 200 "$CREDIT_TYPE"

# 1. Deposit 1000.
req POST "$CREDITS/deposit" "{\"user_id\":\"$USER_ID\",\"credit_type\":\"$CREDIT_TYPE\",\"amount\":1000,\"source\":\"e2e\",\"source_id\":\"$(uuid)\"}"
expect "deposit 1000" 200 '"BalanceAfter":1000'

# 2. Balance (#247): prepaid, 1000 available, 0 held.
req GET "$CREDITS/balance?owner_id=$USER_ID&credit_type=$CREDIT_TYPE"
expect "balance == 1000 prepaid" 200 '"balance_cents":1000'
expect "billing_mode prepaid" 200 '"billing_mode":"prepaid"'

# 3. Atomic authorize+hold 600 within balance (#235/#247) -> allowed + reservation.
req POST "$CREDITS/authorize" "{\"owner_id\":\"$USER_ID\",\"credit_type\":\"$CREDIT_TYPE\",\"estimate_cents\":600,\"request_id\":\"$(uuid)\"}"
expect "authorize+hold 600 allowed" 200 '"allowed":true'
RES=$(jget reservation_id); echo "        reservation_id=$RES"

# 4. Capture 400 of the 600 hold -> remainder released, balance 600.
req POST "$CREDITS/holds/$RES/capture" '{"amount":400}'
expect "capture 400 of hold" 200 '"Status":"captured"'

# 5. Balance now 600, nothing held.
req GET "$CREDITS/balance?owner_id=$USER_ID&credit_type=$CREDIT_TYPE"
expect "balance == 600 after capture" 200 '"balance_cents":600'
expect "held == 0 after capture" 200 '"held_cents":0'

# 6. Authorize OVER balance (5000) -> denied, prepaid insufficient_balance.
req POST "$CREDITS/authorize" "{\"owner_id\":\"$USER_ID\",\"credit_type\":\"$CREDIT_TYPE\",\"estimate_cents\":5000,\"request_id\":\"$(uuid)\"}"
expect "over-balance authorize denied" 200 '"allowed":false'
expect "deny_code insufficient_balance" 200 'insufficient_balance'

# 7. Set arrears mode + outstanding cap (#242).
req PUT "$CREDITS/account-settings" "{\"payer\":\"$USER_ID\",\"credit_type\":\"$CREDIT_TYPE\",\"billing_mode\":\"arrears\",\"max_outstanding_owed_cents\":5000}"
expect "set arrears + cap" 200 '"billing_mode":"arrears"'

# 8. Read settings back -> arrears + cap persisted.
req GET "$CREDITS/account-settings?owner_id=$USER_ID&credit_type=$CREDIT_TYPE"
expect "read settings persisted" 200 '"max_outstanding_owed_cents":5000'

echo "== done: $pass passed, $fail failed =="
[ "$fail" -eq 0 ]
