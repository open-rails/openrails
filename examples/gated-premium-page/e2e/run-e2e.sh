#!/usr/bin/env bash
# Headless six-step demo proof (server-side-token variant). Prereq: provision.sh.
#
# Browser Collect.js tokenization can't run without a browser, so the ONE
# browser-only step is replaced by its server-side equivalent (same pattern as
# tests/nmi_live_lifecycle_e2e_test.go): vault the sandbox card directly at NMI
# and record the payment method row, then drive the REAL checkout API with
# payment_method_id. Everything else is the demo's actual HTTP surface.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
DEMO=$(cd "$HERE/.." && pwd)
REPO=$(cd "$DEMO/../.." && pwd)
STATE=${STATE:-"$HERE/.state"}
BASE=${OPENRAILS_BASE_URL:-http://localhost:3053}
PORT=${DEMO_PORT:-8093}

set -a; source "$REPO/.env"; source "$STATE/apikey.env"; set +a
USER_ID=$(uuidgen | tr 'A-Z' 'a-z')   # fresh user every run
JAR=$(mktemp); trap 'rm -f "$JAR"; kill "$APP_PID" 2>/dev/null || true' EXIT

echo "== start demo app (user $USER_ID)"
(cd "$DEMO" && go build .)
ISSUER_KEY_FILE="$STATE/issuer_key.pem" PORT=$PORT DEMO_USER_ID=$USER_ID \
  "$DEMO/gated-premium-page" >"$STATE/app.log" 2>&1 & APP_PID=$!
sleep 1

step() { echo "-- $*"; }
expect() { [ "$1" = "$2" ] || { echo "FAIL: want $2 got $1 ($3)"; exit 1; }; }

step "1. public page"
expect "$(curl -s -o /dev/null -w '%{http_code}' localhost:$PORT/)" 200 "GET /"
step "2. login"
curl -s -c "$JAR" -o /dev/null localhost:$PORT/login
step "3. /premium without entitlement redirects to /buy"
LOC=$(curl -s -b "$JAR" -o /dev/null -w '%{redirect_url}' localhost:$PORT/premium)
case "$LOC" in */buy) ;; *) echo "FAIL: expected /buy redirect, got $LOC"; exit 1;; esac

step "4. purchase (server-side-token variant)"
TOK=$(curl -sf -b "$JAR" localhost:$PORT/api/token | jq -r .token)
# the premium-monthly key: provision.sh version-bumps it to a fresh random
# amount, which keeps repeat runs outside NMI's duplicate-transaction window
PRICE=$(curl -sf "$BASE/v1/products" | jq -r '[.data[].prices[] | select(.active and .key=="premium-monthly")][0].id')
# materialize the customer row (TouchCustomer) before inserting the payment method
curl -sf -H "Authorization: Bearer $TOK" "$BASE/v1/me/subscriptions" >/dev/null
VAULT=$(curl -s https://secure.networkmerchants.com/api/transact.php \
  --data-urlencode "security_key=$NMI_SANDBOX_SECURITY_KEY" \
  -d "customer_vault=add_customer&ccnumber=4111111111111111&ccexp=1029&cvv=123&first_name=Demo&last_name=User&address1=1 Demo St&city=Testville&state=CA&zip=90001&country=US&email=demo@example.com&test_mode=enabled" |
  tr '&' '\n' | sed -n 's/^customer_vault_id=//p')
[ -n "$VAULT" ] || { echo "FAIL: NMI vault"; exit 1; }
MID=$(docker compose -f "$REPO/docker-compose.yaml" exec -T postgres \
  psql -U admin -d openrails_db -tAc "SELECT id FROM openrails.merchants WHERE slug='demo'")
PM=$(docker compose -f "$REPO/docker-compose.yaml" exec -T postgres \
  psql -U admin -d openrails_db -tAc "INSERT INTO openrails.payment_methods (merchant_id, customer_id, rail, rail_customer_ref, rail_method_ref, rebill_driver, initial_transaction_id, last_four, card_type, expiry_date) VALUES ('$MID','$USER_ID','nmi','$VAULT','','provider','e2e-vault','1111','Visa','10/29') RETURNING id" | head -1)
OUT=$(curl -s -X POST "$BASE/v1/me/checkout" \
  -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -H "Idempotency-Key: $(uuidgen)" \
  -d "{\"price_id\":\"$PRICE\",\"payment\":{\"rail\":\"nmi\",\"payment_method_id\":\"$PM\"}}")
expect "$(echo "$OUT" | jq -r .status)" succeeded "checkout: $OUT"
TXN=$(echo "$OUT" | jq -r .payment.transaction_id)
echo "   NMI approved txn=$TXN sub=$(echo "$OUT" | jq -r .subscription_id)"

step "5. /premium now unlocks"
expect "$(curl -s -b "$JAR" -o /dev/null -w '%{http_code}' localhost:$PORT/premium)" 200 "GET /premium"

step "6a. self view: subscription + entitlement"
expect "$(curl -sf -H "Authorization: Bearer $TOK" "$BASE/v1/me/subscriptions" | jq -r '.data[0].status')" active "me/subscriptions"
expect "$(curl -sf -H "Authorization: Bearer $TOK" "$BASE/v1/me/entitlements/active" | jq -r '.data[0].lookup_key')" premium "me/entitlements"

step "6b. merchant view: payment + entitlement (API key)"
expect "$(curl -sf -H "Authorization: Bearer $OPENRAILS_API_KEY" "$BASE/v1/merchant/payments" | jq -r "[.data[] | select(.transaction_id==\"$TXN\")][0].status")" succeeded "merchant/payments"
expect "$(curl -sf -H "Authorization: Bearer $OPENRAILS_API_KEY" "$BASE/v1/merchant/customers/$USER_ID/entitlements" | jq -r '.[0].entitlement')" premium "merchant entitlements"

step "7. remote state at NMI (query.php)"
curl -s https://secure.nmi.com/api/query.php --data-urlencode "security_key=$NMI_SANDBOX_SECURITY_KEY" \
  -d "report_type=transaction&transaction_id=$TXN" | grep -q "<transaction_id>$TXN</transaction_id>" ||
  { echo "FAIL: NMI query"; exit 1; }

echo "== PASS: gated-premium-page E2E against real NMI sandbox =="
