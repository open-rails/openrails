#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -f "$ROOT_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1090
  source "$ROOT_DIR/.env"
  set +a
fi

COMPOSE_FILE="${COMPOSE_FILE:-$ROOT_DIR/docker-compose.yaml}"
COMPOSE_PROFILES="${COMPOSE_PROFILES:-all,e2e-sandbox}"
BASE_URL="${MOBIUS_E2E_BASE_URL:-http://localhost:2053}"
TOKENIZATION_BASE_URL="${MOBIUS_E2E_TOKENIZATION_BASE_URL:-$BASE_URL}"
START_COMPOSE="${MOBIUS_E2E_START_COMPOSE:-true}"
BUILD_COMPOSE="${MOBIUS_E2E_BUILD:-true}"
START_TUNNEL="${MOBIUS_E2E_START_TUNNEL:-false}"
AUTHKIT_DEV_MINT_SECRET="${AUTHKIT_DEV_MINT_SECRET:-dev-mint-secret-localhost-only}"
AUTHKIT_MINT_URL="${AUTHKIT_MINT_URL:-http://localhost:8080/auth/dev/mint}"
AUTHKIT_AUDIENCE="${AUTHKIT_AUDIENCE:-openrails-app}"
E2E_RUN_ID="${E2E_RUN_ID:-e2e_mobius_live_$(date +%Y%m%dT%H%M%S)_$(cat /proc/sys/kernel/random/uuid 2>/dev/null || date +%s%N)}"
E2E_USER_ID="${E2E_USER_ID:-$(cat /proc/sys/kernel/random/uuid 2>/dev/null || date +%s%N)}"
E2E_EMAIL="${E2E_EMAIL:-e2e+${E2E_RUN_ID}@example.com}"
PLAYWRIGHT_DIR="${PLAYWRIGHT_DIR:-$ROOT_DIR/.runtime/mobius-live-playwright}"
RESULT_DIR="${RESULT_DIR:-$ROOT_DIR/.runtime/mobius-live-results}"

require() {
  local name="$1"
  if ! command -v "$name" >/dev/null 2>&1; then
    echo "Missing required command: $name" >&2
    exit 1
  fi
}

need_env() {
  local name="$1"
  if [ -z "${!name:-}" ]; then
    echo "Missing required env var: $name" >&2
    exit 1
  fi
}

redact_id() {
  local value="${1:-}"
  if [ -z "$value" ]; then
    echo ""
    return
  fi
  if [ "${#value}" -le 10 ]; then
    echo "[redacted]"
    return
  fi
  printf '%s...%s\n' "${value:0:4}" "${value: -4}"
}

curl_json() {
  local method="$1"
  local url="$2"
  local body="${3:-}"
  local out="$4"
  if [ -n "$body" ]; then
    curl -sS -m 60 -o "$out.body" -w '%{http_code}' \
      -X "$method" "$url" \
      -H "Authorization: Bearer $E2E_JWT" \
      -H "Content-Type: application/json" \
      -H "X-E2E-Run-ID: $E2E_RUN_ID" \
      -H "X-Idempotency-Key: e2e_${E2E_RUN_ID}_$(basename "$out")" \
      --data "$body" >"$out.code"
  else
    curl -sS -m 60 -o "$out.body" -w '%{http_code}' \
      -X "$method" "$url" \
      -H "Authorization: Bearer $E2E_JWT" \
      -H "Content-Type: application/json" \
      -H "X-E2E-Run-ID: $E2E_RUN_ID" >"$out.code"
  fi
}

assert_http() {
  local name="$1"
  local want="$2"
  local code_file="$3"
  local body_file="$4"
  local got
  got="$(cat "$code_file")"
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: HTTP $got, wanted $want" >&2
    jq . "$body_file" 2>/dev/null || cat "$body_file" >&2
    exit 1
  fi
  echo "PASS $name [HTTP $got]"
}

psql_scalar() {
  local sql="$1"
  docker compose -f "$COMPOSE_FILE" exec -T postgres \
    psql -U admin -d openrails_db -v ON_ERROR_STOP=1 -Atc "$sql"
}

query_nmi() {
  local report_type="$1"
  local key="$2"
  local value="$3"
  local out="$4"
  curl -fsS -m 60 -X POST "https://secure.nmi.com/api/query.php" \
    -d "security_key=$PROCESSORS_MOBIUS_SECURITY_KEY" \
    -d "report_type=$report_type" \
    -d "$key=$value" >"$out"
  if [ ! -s "$out" ]; then
    echo "FAIL NMI query returned an empty response for $report_type" >&2
    exit 1
  fi
}

cleanup() {
  if [ -n "${TUNNEL_PID:-}" ]; then
    kill "$TUNNEL_PID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

require curl
require docker
require jq
require node
require pnpm

need_env PROCESSORS_MOBIUS_SECURITY_KEY
need_env PROCESSORS_MOBIUS_TOKENIZATION_KEY
need_env PROCESSORS_MOBIUS_TOKENIZATION_URL
need_env PROCESSORS_MOBIUS_WEBHOOK_SECRET
need_env E2E_MOBIUS_PLAN_ID

mkdir -p "$RESULT_DIR"

echo "== OpenRails live Mobius/NMI lifecycle E2E =="
echo "run_id: $E2E_RUN_ID"
echo "user_id: $E2E_USER_ID"
echo "base_url: $BASE_URL"
echo "tokenization_base_url: $TOKENIZATION_BASE_URL"

if [ "$START_COMPOSE" = "true" ]; then
  echo "Starting docker compose stack..."
  PROFILE_ARGS=()
  IFS=',' read -r -a PROFILES <<<"$COMPOSE_PROFILES"
  for p in "${PROFILES[@]}"; do
    p="$(echo "$p" | xargs)"
    if [ -n "$p" ]; then
      PROFILE_ARGS+=(--profile "$p")
    fi
  done
  BUILD_ARGS=()
  if [ "$BUILD_COMPOSE" = "true" ]; then
    BUILD_ARGS+=(--build)
  fi
  AUTHKIT_DEV_MINT_SECRET="$AUTHKIT_DEV_MINT_SECRET" docker compose -f "$COMPOSE_FILE" "${PROFILE_ARGS[@]}" up -d "${BUILD_ARGS[@]}"
fi

echo "Waiting for OpenRails health..."
for _ in $(seq 1 90); do
  if curl -fsS "$BASE_URL/health/live" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -fsS "$BASE_URL/health/live" >/dev/null

if [ "$START_TUNNEL" = "true" ]; then
  require cloudflared
  need_env CLOUDFLARED_TUNNEL_TOKEN
  echo "Starting cloudflared tunnel..."
  cloudflared tunnel run --token "$CLOUDFLARED_TUNNEL_TOKEN" >"$RESULT_DIR/cloudflared.log" 2>&1 &
  TUNNEL_PID="$!"
  sleep 5
  curl -fsS "$TOKENIZATION_BASE_URL/health/live" >/dev/null
fi

echo "Seeding E2E Mobius catalog..."
E2E_MOBIUS_PLAN_ID="$E2E_MOBIUS_PLAN_ID" COMPOSE_FILE="$COMPOSE_FILE" bash "$ROOT_DIR/scripts/seed_e2e_mobius.sh" >/dev/null

RECURRING_PRICE_ID="$(psql_scalar "SELECT id::text FROM billing.prices WHERE product_id = (SELECT id FROM billing.products WHERE slug='e2e_mobius') AND amount=999 AND currency='usd' AND billing_cycle_days=1 ORDER BY created_at DESC LIMIT 1;")"
ONE_OFF_PRICE_ID="$(psql_scalar "SELECT id::text FROM billing.prices WHERE product_id = (SELECT id FROM billing.products WHERE slug='e2e_mobius') AND amount=499 AND currency='usd' AND billing_cycle_days IS NULL ORDER BY created_at DESC LIMIT 1;")"
if [ -z "$RECURRING_PRICE_ID" ] || [ -z "$ONE_OFF_PRICE_ID" ]; then
  echo "Failed to load seeded E2E price ids" >&2
  exit 1
fi

echo "Minting E2E JWT..."
MINT_BODY="$(jq -nc --arg sub "$E2E_USER_ID" --arg aud "$AUTHKIT_AUDIENCE" --arg email "$E2E_EMAIL" '{sub:$sub,aud:$aud,email:$email,expires_in_seconds:3600}')"
for i in $(seq 1 60); do
  if MINT_RAW="$(curl -fsS "$AUTHKIT_MINT_URL" -H "Authorization: Bearer $AUTHKIT_DEV_MINT_SECRET" -H "Content-Type: application/json" --data "$MINT_BODY" 2>/dev/null)"; then
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "AuthKit dev issuer did not mint a JWT at $AUTHKIT_MINT_URL" >&2
    exit 1
  fi
  sleep 1
done
E2E_JWT="$(printf '%s' "$MINT_RAW" | jq -r '.token // empty')"
if [ -z "$E2E_JWT" ]; then
  echo "Mint response did not contain .token" >&2
  exit 1
fi

if [ ! -d "$PLAYWRIGHT_DIR/node_modules/playwright" ]; then
  echo "Installing Playwright into $PLAYWRIGHT_DIR..."
  mkdir -p "$PLAYWRIGHT_DIR"
  if [ ! -f "$PLAYWRIGHT_DIR/package.json" ]; then
    printf '{"private":true,"dependencies":{}}\n' >"$PLAYWRIGHT_DIR/package.json"
  fi
  pnpm --dir "$PLAYWRIGHT_DIR" add playwright >/dev/null
fi
if [ ! -f "$PLAYWRIGHT_DIR/.chromium-installed" ]; then
  echo "Installing Playwright Chromium browser..."
  pnpm --dir "$PLAYWRIGHT_DIR" exec playwright install chromium >/dev/null
  touch "$PLAYWRIGHT_DIR/.chromium-installed"
fi

echo "Creating saved payment method through real Collect.js..."
NODE_PATH="$PLAYWRIGHT_DIR/node_modules" \
TOKENIZATION_BASE_URL="$TOKENIZATION_BASE_URL" \
E2E_JWT="$E2E_JWT" \
E2E_RUN_ID="$E2E_RUN_ID" \
RESULT_DIR="$RESULT_DIR" \
node <<'JS'
const { chromium } = require('playwright');
const fs = require('fs');

const resultPath = process.env.RESULT_DIR ? `${process.env.RESULT_DIR}/payment_method.json` : '.runtime/mobius-live-results/payment_method.json';

function frameKind(frame) {
  const url = frame.url();
  if (url.includes('elementId=ccnumber')) return 'ccnumber';
  if (url.includes('elementId=ccexp')) return 'ccexp';
  if (url.includes('elementId=cvv')) return 'cvv';
  return '';
}

async function hostedFrame(page, kind) {
  const deadline = Date.now() + 20000;
  while (Date.now() < deadline) {
    const frame = page.frames().find((f) => frameKind(f) === kind);
    if (frame) return frame;
    await page.waitForTimeout(250);
  }
  throw new Error(`missing hosted ${kind} frame`);
}

async function fillHosted(page, kind, selector, value) {
  const frame = await hostedFrame(page, kind);
  const input = frame.locator(selector);
  await input.waitFor({ state: 'visible', timeout: 10000 });
  await input.fill(value);
}

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();
  await page.goto(`${process.env.TOKENIZATION_BASE_URL.replace(/\/$/, '')}/debug/nmi/tokenization?mode=real&provider=mobius`, { waitUntil: 'domcontentloaded' });
  await page.fill('#jwt', process.env.E2E_JWT);
  await page.fill('#e2e_run_id', process.env.E2E_RUN_ID);
  await page.fill('#first_name', 'OpenRails');
  await page.fill('#last_name', 'MobiusE2E');
  await page.fill('#address1', '888 Test St');
  await page.fill('#city', 'Testville');
  await page.fill('#state', 'CA');
  await page.fill('#zip', '77777');
  await page.fill('#country', 'US');
  await page.fill('#email', `mobius-live-${process.env.E2E_RUN_ID}@example.com`);

  await page.click('#btn-config');
  await page.waitForFunction(() => document.getElementById('status')?.textContent?.includes('configured'), null, { timeout: 20000 });
  await fillHosted(page, 'ccnumber', '#ccnumber', '4111111111111111');
  await fillHosted(page, 'ccexp', '#ccexp', '1228');
  await fillHosted(page, 'cvv', '#cvv', '123');
  await page.click('#btn-token');
  await page.waitForFunction(() => (document.getElementById('token')?.value || '').trim().length > 0, null, { timeout: 30000 });
  await page.click('#btn-create-pm');
  await page.waitForFunction(() => (document.getElementById('api-result')?.value || '').startsWith('HTTP '), null, { timeout: 60000 });

  const apiResult = await page.locator('#api-result').inputValue();
  const status = apiResult.match(/^HTTP\s+(\d+)/)?.[1] || '';
  const body = apiResult.replace(/^HTTP\s+\d+(?:\\n|\n)/, '');
  let parsed;
  try {
    parsed = JSON.parse(body);
  } catch (err) {
    throw new Error(`payment method response was not JSON: ${apiResult}`);
  }
  if (status !== '200' || !parsed.id) {
    throw new Error(`payment method create failed: HTTP ${status} ${body}`);
  }
  fs.writeFileSync(resultPath, JSON.stringify({
    id: parsed.id,
    processor: parsed.processor,
    type: parsed.type,
    billing_details: parsed.billing_details,
    metadata_keys: parsed.metadata ? Object.keys(parsed.metadata).sort() : [],
  }, null, 2), { mode: 0o600 });
  await browser.close();
})();
JS

PAYMENT_METHOD_ID="$(jq -r '.id' "$RESULT_DIR/payment_method.json")"
echo "PASS saved payment method created: $PAYMENT_METHOD_ID"

curl_json GET "$BASE_URL/v1/me/payment-methods" "" "$RESULT_DIR/payment_methods_list"
assert_http "payment method list readback" 200 "$RESULT_DIR/payment_methods_list.code" "$RESULT_DIR/payment_methods_list.body"
if ! jq -e --arg id "$PAYMENT_METHOD_ID" '.data[]? | select(.id == $id and .processor == "mobius" and .type == "card")' "$RESULT_DIR/payment_methods_list.body" >/dev/null; then
  echo "FAIL saved payment method was not returned by list readback" >&2
  jq . "$RESULT_DIR/payment_methods_list.body" >&2
  exit 1
fi
echo "PASS saved payment method list readback"

LOCAL_PM_CHECK="$(psql_scalar "SELECT concat(count(*), '|', bool_and(vault_id IS NOT NULL AND vault_id <> ''), '|', bool_and(coalesce(metadata::text,'') NOT LIKE '%411111%' AND lower(coalesce(metadata::text,'')) NOT LIKE '%cvv%')) FROM billing.payment_methods WHERE metadata->>'e2e_run_id' = '$E2E_RUN_ID';")"
if [ "$LOCAL_PM_CHECK" != "1|t|t" ]; then
  echo "FAIL saved payment method persistence/no-raw-card check: $LOCAL_PM_CHECK" >&2
  exit 1
fi
echo "PASS saved payment method persisted with vault id and no raw card metadata"

echo "Charging saved card through one-off checkout..."
ONE_OFF_BODY="$(jq -nc --arg price "$ONE_OFF_PRICE_ID" --arg pm "$PAYMENT_METHOD_ID" --arg run "$E2E_RUN_ID" '{price_id:$price,mode:"one_off",metadata:{e2e_run_id:$run},payment:{processor:"mobius",payment_method_id:$pm}}')"
curl_json POST "$BASE_URL/v1/checkout" "$ONE_OFF_BODY" "$RESULT_DIR/one_off_checkout"
assert_http "one-off checkout" 200 "$RESULT_DIR/one_off_checkout.code" "$RESULT_DIR/one_off_checkout.body"
ONE_OFF_STATUS="$(jq -r '.status' "$RESULT_DIR/one_off_checkout.body")"
ONE_OFF_TXN_ID="$(jq -r '.payment.transaction_id // empty' "$RESULT_DIR/one_off_checkout.body")"
if [ "$ONE_OFF_STATUS" != "succeeded" ] || [ -z "$ONE_OFF_TXN_ID" ]; then
  echo "FAIL one-off checkout did not succeed with a transaction id" >&2
  jq . "$RESULT_DIR/one_off_checkout.body" >&2
  exit 1
fi
echo "PASS one-off saved-vault charge: $(redact_id "$ONE_OFF_TXN_ID")"

ONE_OFF_DB_CHECK="$(psql_scalar "SELECT concat(count(*), '|', bool_and(status='completed'), '|', bool_and(amount=499), '|', bool_and(currency='usd')) FROM billing.payments WHERE metadata->>'e2e_run_id' = '$E2E_RUN_ID' AND transaction_id = '$ONE_OFF_TXN_ID';")"
if [ "$ONE_OFF_DB_CHECK" != "1|t|t|t" ]; then
  echo "FAIL one-off payment DB check: $ONE_OFF_DB_CHECK" >&2
  exit 1
fi
echo "PASS one-off local payment row matches amount/currency/status"

echo "Creating subscription checkout with the same saved card..."
SUB_BODY="$(jq -nc --arg price "$RECURRING_PRICE_ID" --arg pm "$PAYMENT_METHOD_ID" --arg run "$E2E_RUN_ID" '{price_id:$price,mode:"subscription",metadata:{e2e_run_id:$run},payment:{processor:"mobius",payment_method_id:$pm}}')"
curl_json POST "$BASE_URL/v1/checkout" "$SUB_BODY" "$RESULT_DIR/subscription_checkout"
assert_http "subscription checkout" 200 "$RESULT_DIR/subscription_checkout.code" "$RESULT_DIR/subscription_checkout.body"
SUB_STATUS="$(jq -r '.status' "$RESULT_DIR/subscription_checkout.body")"
SUBSCRIPTION_ID="$(jq -r '.subscription_id // empty' "$RESULT_DIR/subscription_checkout.body")"
SUB_TXN_ID="$(jq -r '.payment.transaction_id // empty' "$RESULT_DIR/subscription_checkout.body")"
if [ "$SUB_STATUS" != "succeeded" ] && [ "$SUB_STATUS" != "pending" ]; then
  echo "FAIL subscription checkout returned unexpected status: $SUB_STATUS" >&2
  jq . "$RESULT_DIR/subscription_checkout.body" >&2
  exit 1
fi
if [ -z "$SUBSCRIPTION_ID" ] || [ -z "$SUB_TXN_ID" ]; then
  echo "FAIL subscription checkout did not return subscription id and transaction id" >&2
  jq . "$RESULT_DIR/subscription_checkout.body" >&2
  exit 1
fi
echo "PASS subscription checkout created: $SUBSCRIPTION_ID txn=$(redact_id "$SUB_TXN_ID")"

SUB_UUID="${SUBSCRIPTION_ID#sub_}"
PROVIDER_SUB_ID="$(psql_scalar "SELECT processor_subscription_id FROM billing.subscriptions WHERE id = '$SUB_UUID'::uuid AND gateway_response->>'e2e_run_id' = '$E2E_RUN_ID' LIMIT 1;")"
if [ -z "$PROVIDER_SUB_ID" ]; then
  echo "FAIL subscription local row missing provider subscription id" >&2
  exit 1
fi
echo "PASS subscription local row has provider subscription id: $(redact_id "$PROVIDER_SUB_ID")"

echo "Verifying remote NMI state via Query API..."
query_nmi transaction transaction_id "$ONE_OFF_TXN_ID" "$RESULT_DIR/nmi_one_off_query.xml"
query_nmi transaction transaction_id "$SUB_TXN_ID" "$RESULT_DIR/nmi_subscription_txn_query.xml"
query_nmi recurring recurring_id "$PROVIDER_SUB_ID" "$RESULT_DIR/nmi_recurring_query.xml"
echo "PASS NMI Query API returned transaction and recurring responses"

echo "Cancelling subscription through OpenRails..."
CANCEL_BODY='{"feedback":"mobius live e2e cancel"}'
curl_json POST "$BASE_URL/v1/me/subscriptions/$SUBSCRIPTION_ID/cancel" "$CANCEL_BODY" "$RESULT_DIR/cancel_subscription"
assert_http "subscription cancel" 200 "$RESULT_DIR/cancel_subscription.code" "$RESULT_DIR/cancel_subscription.body"
CANCEL_CHECK="$(psql_scalar "SELECT concat(count(*), '|', bool_and(status='cancelled')) FROM billing.subscriptions WHERE id = '$SUB_UUID'::uuid;")"
if [ "$CANCEL_CHECK" != "1|t" ]; then
  echo "FAIL local cancellation check: $CANCEL_CHECK" >&2
  exit 1
fi
query_nmi recurring recurring_id "$PROVIDER_SUB_ID" "$RESULT_DIR/nmi_recurring_after_cancel_query.xml"
echo "PASS subscription cancelled locally and remote recurring query still resolves"

SUMMARY="$RESULT_DIR/summary.json"
jq -nc \
  --arg run_id "$E2E_RUN_ID" \
  --arg user_id "$E2E_USER_ID" \
  --arg payment_method_id "$PAYMENT_METHOD_ID" \
  --arg one_off_transaction_id "$ONE_OFF_TXN_ID" \
  --arg subscription_id "$SUBSCRIPTION_ID" \
  --arg provider_subscription_id "$PROVIDER_SUB_ID" \
  --arg subscription_transaction_id "$SUB_TXN_ID" \
  '{
    run_id:$run_id,
    user_id:$user_id,
    payment_method_id:$payment_method_id,
    checks:{
      hosted_tokenization:true,
      saved_payment_method:true,
      payment_method_list_readback:true,
      saved_vault_one_off_charge:true,
      saved_vault_subscription_checkout:true,
      nmi_query:true,
      cancellation:true
    },
    provider_refs:{
      one_off_transaction_id:$one_off_transaction_id,
      subscription_id:$provider_subscription_id,
      subscription_transaction_id:$subscription_transaction_id
    },
    local_refs:{subscription_id:$subscription_id}
  }' >"$SUMMARY"

echo "== PASS live Mobius/NMI lifecycle E2E =="
echo "summary: $SUMMARY"
