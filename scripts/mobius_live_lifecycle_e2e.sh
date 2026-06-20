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
COMPOSE_PROFILES="${COMPOSE_PROFILES:-all}"
OPENRAILS_HOST_PORT="${OPENRAILS_HOST_PORT:-3053}"
BASE_URL="${NMI_E2E_BASE_URL:-http://localhost:$OPENRAILS_HOST_PORT}"
START_COMPOSE="${NMI_E2E_START_COMPOSE:-true}"
BUILD_COMPOSE="${NMI_E2E_BUILD:-true}"
START_TUNNEL="${NMI_E2E_START_TUNNEL:-false}"
E2E_NMI_MERCHANT_SLUG="${E2E_NMI_MERCHANT_SLUG:-local-stack}"
E2E_NMI_ISSUER="${E2E_NMI_ISSUER:-https://local-stack-e2e.example}"
E2E_NMI_ORIGIN="${E2E_NMI_ORIGIN:-https://local-stack-e2e.example}"
E2E_NMI_AUDIENCE="${E2E_NMI_AUDIENCE:-openrails}"
E2E_RUN_ID="${E2E_RUN_ID:-e2e_nmi_live_$(date +%Y%m%dT%H%M%S)_$(cat /proc/sys/kernel/random/uuid 2>/dev/null || date +%s%N)}"
E2E_USER_ID="${E2E_USER_ID:-$(cat /proc/sys/kernel/random/uuid 2>/dev/null || date +%s%N)}"
E2E_EMAIL="${E2E_EMAIL:-e2e+${E2E_RUN_ID}@example.com}"
E2E_NMI_PLAN_ID="${E2E_NMI_PLAN_ID:-openrails_e2e_nmi_daily_999}"
E2E_NMI_ONE_OFF_AMOUNT="${E2E_NMI_ONE_OFF_AMOUNT:-$((500 + ($(date +%s) % 400)))}"
E2E_NMI_RECURRING_AMOUNT="${E2E_NMI_RECURRING_AMOUNT:-$((1000 + ($(date +%s) % 400)))}"
RESULT_DIR="${RESULT_DIR:-$ROOT_DIR/.runtime/nmi-live-results}"
PLAYWRIGHT_DIR="${PLAYWRIGHT_DIR:-$ROOT_DIR/.runtime/nmi-live-playwright}"
NMI_SANDBOX_SECURITY_KEY="${NMI_SANDBOX_SECURITY_KEY:-}"
NMI_TOKENIZATION_KEY="${NMI_TOKENIZATION_KEY:-}"
NMI_COLLECT_JS_URL="${NMI_COLLECT_JS_URL:-https://secure.networkmerchants.com/token/Collect.js}"
NMI_E2E_CARD_NUMBER="${NMI_E2E_CARD_NUMBER:-4111111111111111}"
NMI_E2E_CARD_EXPIRY="${NMI_E2E_CARD_EXPIRY:-1228}"
NMI_E2E_CARD_CVV="${NMI_E2E_CARD_CVV:-123}"
NMI_WEBHOOK_SIGNING_SECRET="${NMI_WEBHOOK_SIGNING_SECRET:-}"

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

assert_http_accepted_or_ok() {
  local name="$1"
  local code_file="$2"
  local body_file="$3"
  local got
  got="$(cat "$code_file")"
  if [ "$got" != "200" ] && [ "$got" != "202" ]; then
    echo "FAIL $name: HTTP $got, wanted 200 or 202" >&2
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
    -d "security_key=$NMI_SANDBOX_SECURITY_KEY" \
    -d "report_type=$report_type" \
    -d "$key=$value" >"$out"
  if [ ! -s "$out" ]; then
    echo "FAIL NMI query returned an empty response for $report_type" >&2
    exit 1
  fi
}

assert_query_contains() {
  local name="$1"
  local file="$2"
  local needle="$3"
  if ! grep -Fq "$needle" "$file"; then
    echo "FAIL $name: NMI query response did not contain expected value: $(redact_id "$needle")" >&2
    sed -E 's/[A-Za-z0-9_-]{25,}/[redacted]/g' "$file" | head -80 >&2
    exit 1
  fi
}

assert_query_status() {
  local name="$1"
  local file="$2"
  if ! grep -Eiq '(complete|completed|approved|success|active|cancelled|canceled)' "$file"; then
    echo "FAIL $name: NMI query response did not include an approved/active/cancelled status" >&2
    sed -E 's/[A-Za-z0-9_-]{25,}/[redacted]/g' "$file" | head -80 >&2
    exit 1
  fi
}

assert_query_currency_if_present() {
  local name="$1"
  local file="$2"
  if grep -Eiq 'currency' "$file" && ! grep -Eiq 'usd' "$file"; then
    echo "FAIL $name: NMI query response included currency metadata but not USD" >&2
    sed -E 's/[A-Za-z0-9_-]{25,}/[redacted]/g' "$file" | head -80 >&2
    exit 1
  fi
}

sign_nmi_webhook_header() {
  local body="$1"
  local timestamp
  timestamp="$(date +%s)"
  local signature
  signature="$(printf '%s.%s' "$timestamp" "$body" | openssl dgst -sha256 -hmac "$NMI_WEBHOOK_SIGNING_SECRET" | awk '{print $NF}')"
  printf 't=%s,s=%s' "$timestamp" "$signature"
}

post_signed_nmi_webhook() {
  local body="$1"
  local out="$2"
  local signature
  signature="$(sign_nmi_webhook_header "$body")"
  curl -sS -m 60 -o "$out.body" -w '%{http_code}' \
    -X POST "$BASE_URL/v1/merchants/$E2E_NMI_MERCHANT_SLUG/webhooks/mobius" \
    -H "Content-Type: application/json" \
    -H "Webhook-Signature: $signature" \
    --data "$body" >"$out.code"
}

cleanup() {
  if [ -n "${TUNNEL_PID:-}" ]; then
    kill "$TUNNEL_PID" >/dev/null 2>&1 || true
  fi
  if [ -n "${TMPDIR:-}" ]; then
    rm -rf "$TMPDIR"
  fi
}
trap cleanup EXIT

require curl
require docker
require jq
require node
require openssl
require pnpm
require go

need_env NMI_SANDBOX_SECURITY_KEY
need_env NMI_TOKENIZATION_KEY
need_env NMI_WEBHOOK_SIGNING_SECRET

mkdir -p "$RESULT_DIR"
TMPDIR="$(mktemp -d)"

cat >"$TMPDIR/token_tool.go" <<'GO'
package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	authcore "github.com/open-rails/authkit/core"
	jwtkit "github.com/open-rails/authkit/jwt"
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) != 7 {
		fatalf("usage: token_tool KID PRIVATE_PEM PUBLIC_PEM ISSUER AUDIENCE SUBJECT")
	}
	signer, err := jwtkit.NewRSASigner(2048, os.Args[1])
	if err != nil {
		fatalf("new signer: %v", err)
	}
	priv := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(signer.PrivateKey())})
	if err := os.WriteFile(os.Args[2], priv, 0600); err != nil {
		fatalf("write private key: %v", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(signer.PublicKey())
	if err != nil {
		fatalf("marshal public key: %v", err)
	}
	pub := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	if err := os.WriteFile(os.Args[3], pub, 0600); err != nil {
		fatalf("write public key: %v", err)
	}
	tok, err := authcore.MintDelegatedAccessToken(context.Background(), signer, authcore.DelegatedAccessParams{
		Issuer:           os.Args[4],
		Audiences:        []string{os.Args[5]},
		DelegatedSubject: os.Args[6],
	})
	if err != nil {
		fatalf("mint token: %v", err)
	}
	fmt.Println(tok)
}
GO

echo "== OpenRails live NMI lifecycle E2E =="
echo "run_id: $E2E_RUN_ID"
echo "user_id: $E2E_USER_ID"
echo "base_url: $BASE_URL"
echo "one_off_amount_cents: $E2E_NMI_ONE_OFF_AMOUNT"
echo "recurring_amount_cents: $E2E_NMI_RECURRING_AMOUNT"

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
  OPENRAILS_HOST_PORT="$OPENRAILS_HOST_PORT" API_URL="$BASE_URL" docker compose -f "$COMPOSE_FILE" "${PROFILE_ARGS[@]}" up -d "${BUILD_ARGS[@]}"
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
	curl -fsS "$BASE_URL/health/live" >/dev/null
fi

echo "Provisioning local-stack NMI merchant..."
TOKEN_KID="openrails-nmi-e2e"
E2E_JWT="$(go run "$TMPDIR/token_tool.go" "$TOKEN_KID" "$TMPDIR/private.pem" "$TMPDIR/public.pem" "$E2E_NMI_ISSUER" "$E2E_NMI_AUDIENCE" "$E2E_USER_ID")"
{
  cat <<YAML
version: 1
merchants:
  - slug: $E2E_NMI_MERCHANT_SLUG
    display_name: Local Stack
    issuer:
      uri: $E2E_NMI_ISSUER
      audiences:
        - $E2E_NMI_AUDIENCE
      allowed_origins:
        - $E2E_NMI_ORIGIN
      public_keys:
        - kid: $TOKEN_KID
          public_key_pem: |
YAML
  sed 's/^/            /' "$TMPDIR/public.pem"
  cat <<YAML
    provider_accounts:
      - provider_type: nmi
        environment: live
        account_id: nmi-local-stack-e2e
        mode: primary
        secrets:
          security_key:
            env: NMI_SANDBOX_SECURITY_KEY
          tokenization_key:
            env: NMI_TOKENIZATION_KEY
          webhook_signing_secret:
            env: NMI_WEBHOOK_SIGNING_SECRET
YAML
} >"$TMPDIR/merchant.yaml"
docker compose -f "$COMPOSE_FILE" exec -T \
  -e NMI_SANDBOX_SECURITY_KEY="$NMI_SANDBOX_SECURITY_KEY" \
  -e NMI_TOKENIZATION_KEY="$NMI_TOKENIZATION_KEY" \
  -e NMI_WEBHOOK_SIGNING_SECRET="$NMI_WEBHOOK_SIGNING_SECRET" \
  openrails sh -c 'cat > /tmp/nmi-e2e-merchant.yaml && openrails push-merchant-config --file /tmp/nmi-e2e-merchant.yaml --insert --overwrite >/tmp/nmi-e2e-merchant.out && cat /tmp/nmi-e2e-merchant.out' <"$TMPDIR/merchant.yaml" >/dev/null
echo "PASS local-stack NMI merchant provisioned"
if [ "$START_COMPOSE" = "true" ]; then
  echo "Restarting OpenRails to reload delegated issuer registry..."
  docker compose -f "$COMPOSE_FILE" restart openrails >/dev/null
  for _ in $(seq 1 90); do
    if curl -fsS "$BASE_URL/health/live" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  curl -fsS "$BASE_URL/health/live" >/dev/null
fi

RECURRING_AMOUNT_DECIMAL="$(printf '%d.%02d' "$((E2E_NMI_RECURRING_AMOUNT / 100))" "$((E2E_NMI_RECURRING_AMOUNT % 100))")"
echo "Ensuring NMI sandbox recurring plan exists: $(redact_id "$E2E_NMI_PLAN_ID")"
ADD_PLAN_OUT="$RESULT_DIR/nmi_add_plan.txt"
curl -fsS -m 60 -X POST "https://secure.networkmerchants.com/api/transact.php" \
  -d "security_key=$NMI_SANDBOX_SECURITY_KEY" \
  -d "recurring=add_plan" \
  -d "plan_id=$E2E_NMI_PLAN_ID" \
  -d "plan_name=OpenRails E2E Daily 999" \
  -d "plan_amount=$RECURRING_AMOUNT_DECIMAL" \
  -d "day_frequency=1" \
  -d "plan_payments=0" >"$ADD_PLAN_OUT"
if grep -q 'response=1' "$ADD_PLAN_OUT"; then
  echo "PASS NMI recurring plan created: $(redact_id "$E2E_NMI_PLAN_ID")"
elif grep -Eiq 'already.*(exist|use|used|using)|duplicate' "$ADD_PLAN_OUT"; then
  echo "PASS NMI recurring plan already exists: $(redact_id "$E2E_NMI_PLAN_ID")"
else
  echo "FAIL could not ensure NMI recurring plan" >&2
  sed -E 's/[A-Za-z0-9_-]{25,}/[redacted]/g' "$ADD_PLAN_OUT" >&2
  exit 1
fi

echo "Seeding E2E NMI catalog..."
E2E_NMI_MERCHANT_SLUG="$E2E_NMI_MERCHANT_SLUG" E2E_NMI_PLAN_ID="$E2E_NMI_PLAN_ID" E2E_NMI_ONE_OFF_AMOUNT="$E2E_NMI_ONE_OFF_AMOUNT" E2E_NMI_RECURRING_AMOUNT="$E2E_NMI_RECURRING_AMOUNT" COMPOSE_FILE="$COMPOSE_FILE" bash "$ROOT_DIR/scripts/seed_e2e_mobius.sh" >/dev/null

RECURRING_PRICE_ID="$(psql_scalar "SELECT price.id::text FROM openrails.prices price JOIN openrails.products product ON product.id = price.product_id JOIN openrails.merchants merchant ON merchant.id = product.merchant_id WHERE merchant.slug='$E2E_NMI_MERCHANT_SLUG' AND product.slug='e2e_mobius' AND price.amount=$E2E_NMI_RECURRING_AMOUNT AND price.currency='usd' AND price.billing_cycle_days=1 ORDER BY price.created_at DESC LIMIT 1;")"
ONE_OFF_PRICE_ID="$(psql_scalar "SELECT price.id::text FROM openrails.prices price JOIN openrails.products product ON product.id = price.product_id JOIN openrails.merchants merchant ON merchant.id = product.merchant_id WHERE merchant.slug='$E2E_NMI_MERCHANT_SLUG' AND product.slug='e2e_mobius' AND price.amount=$E2E_NMI_ONE_OFF_AMOUNT AND price.currency='usd' AND price.billing_cycle_days IS NULL ORDER BY price.created_at DESC LIMIT 1;")"
if [ -z "$RECURRING_PRICE_ID" ] || [ -z "$ONE_OFF_PRICE_ID" ]; then
  echo "Failed to load seeded E2E price ids" >&2
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

echo "Generating NMI payment token through Collect.js..."
NODE_PATH="$PLAYWRIGHT_DIR/node_modules" \
RESULT_DIR="$RESULT_DIR" \
NMI_TOKENIZATION_KEY="$NMI_TOKENIZATION_KEY" \
NMI_COLLECT_JS_URL="$NMI_COLLECT_JS_URL" \
NMI_E2E_CARD_NUMBER="$NMI_E2E_CARD_NUMBER" \
NMI_E2E_CARD_EXPIRY="$NMI_E2E_CARD_EXPIRY" \
NMI_E2E_CARD_CVV="$NMI_E2E_CARD_CVV" \
node <<'JS'
const { chromium } = require('playwright');
const fs = require('fs');
const http = require('http');

const resultPath = `${process.env.RESULT_DIR}/payment_token.json`;
const tokenizationKey = process.env.NMI_TOKENIZATION_KEY || '';
const collectJsURL = process.env.NMI_COLLECT_JS_URL || 'https://secure.networkmerchants.com/token/Collect.js';
const cardNumber = process.env.NMI_E2E_CARD_NUMBER || '4111111111111111';
const cardExpiry = process.env.NMI_E2E_CARD_EXPIRY || '1228';
const cardCVV = process.env.NMI_E2E_CARD_CVV || '123';

if (!tokenizationKey.trim()) throw new Error('NMI_TOKENIZATION_KEY is required');

const html = `<!doctype html>
<html><head><meta charset="utf-8"><title>NMI E2E Tokenization</title></head>
<body>
  <input id="ccnumber" autocomplete="cc-number">
  <input id="ccexp" autocomplete="cc-exp">
  <input id="cvv" autocomplete="cc-csc">
  <script src="${collectJsURL}" data-tokenization-key="${tokenizationKey}"></script>
</body></html>`;

function frameKind(frame) {
  const url = frame.url();
  if (url.includes('elementId=ccnumber')) return 'ccnumber';
  if (url.includes('elementId=ccexp')) return 'ccexp';
  if (url.includes('elementId=cvv')) return 'cvv';
  return '';
}

async function hostedFrame(page, kind) {
  const deadline = Date.now() + 30000;
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
  const server = http.createServer((req, res) => {
    res.writeHead(200, {'content-type': 'text/html; charset=utf-8'});
    res.end(html);
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const url = `http://127.0.0.1:${server.address().port}/`;

  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    await page.goto(url, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => Boolean(window.CollectJS), null, { timeout: 30000 });
    await page.evaluate(() => {
      window.__nmiToken = '';
      window.__nmiTokenResponse = null;
      window.CollectJS.configure({
        variant: 'inline',
        fields: {
          ccnumber: { selector: '#ccnumber', placeholder: '0000 0000 0000 0000' },
          ccexp: { selector: '#ccexp', placeholder: 'MMYY' },
          cvv: { selector: '#cvv', placeholder: '123' },
        },
        callback: (resp) => {
          window.__nmiTokenResponse = resp || null;
          window.__nmiToken = resp && resp.token ? String(resp.token) : '';
        },
      });
    });
    await fillHosted(page, 'ccnumber', '#ccnumber', cardNumber);
    await fillHosted(page, 'ccexp', '#ccexp', cardExpiry);
    await fillHosted(page, 'cvv', '#cvv', cardCVV);
    await page.evaluate(() => window.CollectJS.startPaymentRequest());
    await page.waitForFunction(() => (window.__nmiToken || '').trim().length > 0, null, { timeout: 30000 });
    const token = await page.evaluate(() => window.__nmiToken);
    fs.writeFileSync(resultPath, JSON.stringify({ payment_token: token }, null, 2), { mode: 0o600 });
  } finally {
    await browser.close();
    await new Promise((resolve) => server.close(resolve));
  }
})();
JS
PAYMENT_TOKEN="$(jq -r '.payment_token // empty' "$RESULT_DIR/payment_token.json")"
if [ -z "$PAYMENT_TOKEN" ]; then
  echo "FAIL Collect.js did not produce a payment token" >&2
  exit 1
fi
echo "PASS NMI payment token generated through Collect.js"

echo "Creating saved payment method from generated payment token..."
PAYMENT_METHOD_BODY="$(jq -nc --arg token "$PAYMENT_TOKEN" --arg email "nmi-live-${E2E_RUN_ID}@example.com" '{payment_token:$token,provider:"mobius",first_name:"OpenRails",last_name:"NMIE2E",address1:"888 Test St",city:"Testville",state:"CA",zip:"77777",country:"US",email:$email}')"
curl_json POST "$BASE_URL/v1/me/payment-methods" "$PAYMENT_METHOD_BODY" "$RESULT_DIR/payment_method"
assert_http "payment method create" 200 "$RESULT_DIR/payment_method.code" "$RESULT_DIR/payment_method.body"
jq '{id,processor,type,billing_details,metadata_keys:(.metadata // {} | keys | sort)}' "$RESULT_DIR/payment_method.body" >"$RESULT_DIR/payment_method.json"

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

CARD_PREFIX="$(printf '%s' "$NMI_E2E_CARD_NUMBER" | tr -cd '0-9' | cut -c1-6)"
LOCAL_PM_CHECK="$(psql_scalar "SELECT concat(count(*), '|', bool_and(vault_id IS NOT NULL AND vault_id <> ''), '|', bool_and(coalesce(metadata::text,'') NOT LIKE '%$CARD_PREFIX%' AND lower(coalesce(metadata::text,'')) NOT LIKE '%cvv%')) FROM openrails.payment_methods WHERE metadata->>'e2e_run_id' = '$E2E_RUN_ID';")"
if [ "$LOCAL_PM_CHECK" != "1|t|t" ]; then
  echo "FAIL saved payment method persistence/no-raw-card check: $LOCAL_PM_CHECK" >&2
  exit 1
fi
echo "PASS saved payment method persisted with vault id and no raw card metadata"

echo "Charging saved card through one-off checkout..."
ONE_OFF_BODY="$(jq -nc --arg price "$ONE_OFF_PRICE_ID" --arg pm "$PAYMENT_METHOD_ID" --arg run "$E2E_RUN_ID" '{price_id:$price,mode:"one_off",metadata:{e2e_run_id:$run},payment:{processor:"mobius",payment_method_id:$pm}}')"
curl_json POST "$BASE_URL/v1/me/checkout" "$ONE_OFF_BODY" "$RESULT_DIR/one_off_checkout"
assert_http "one-off checkout" 200 "$RESULT_DIR/one_off_checkout.code" "$RESULT_DIR/one_off_checkout.body"
ONE_OFF_STATUS="$(jq -r '.status' "$RESULT_DIR/one_off_checkout.body")"
ONE_OFF_TXN_ID="$(jq -r '.payment.transaction_id // empty' "$RESULT_DIR/one_off_checkout.body")"
if [ "$ONE_OFF_STATUS" != "succeeded" ] || [ -z "$ONE_OFF_TXN_ID" ]; then
  echo "FAIL one-off checkout did not succeed with a transaction id" >&2
  jq . "$RESULT_DIR/one_off_checkout.body" >&2
  exit 1
fi
echo "PASS one-off saved-vault charge: $(redact_id "$ONE_OFF_TXN_ID")"

ONE_OFF_DB_CHECK="$(psql_scalar "SELECT concat(count(*), '|', bool_and(status='completed'), '|', bool_and(amount=$E2E_NMI_ONE_OFF_AMOUNT), '|', bool_and(currency='usd')) FROM openrails.payments WHERE metadata->>'e2e_run_id' = '$E2E_RUN_ID' AND transaction_id = '$ONE_OFF_TXN_ID';")"
if [ "$ONE_OFF_DB_CHECK" != "1|t|t|t" ]; then
  echo "FAIL one-off payment DB check: $ONE_OFF_DB_CHECK" >&2
  exit 1
fi
echo "PASS one-off local payment row matches amount/currency/status"

echo "Creating subscription checkout with the same saved card..."
SUB_BODY="$(jq -nc --arg price "$RECURRING_PRICE_ID" --arg pm "$PAYMENT_METHOD_ID" --arg run "$E2E_RUN_ID" '{price_id:$price,mode:"subscription",metadata:{e2e_run_id:$run},payment:{processor:"mobius",payment_method_id:$pm}}')"
curl_json POST "$BASE_URL/v1/me/checkout" "$SUB_BODY" "$RESULT_DIR/subscription_checkout"
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
PROVIDER_SUB_ID="$(psql_scalar "SELECT processor_subscription_id FROM openrails.subscriptions WHERE id = '$SUB_UUID'::uuid AND gateway_response->>'e2e_run_id' = '$E2E_RUN_ID' LIMIT 1;")"
if [ -z "$PROVIDER_SUB_ID" ]; then
  echo "FAIL subscription local row missing provider subscription id" >&2
  exit 1
fi
echo "PASS subscription local row has provider subscription id: $(redact_id "$PROVIDER_SUB_ID")"

echo "Verifying remote NMI state via Query API..."
query_nmi transaction transaction_id "$ONE_OFF_TXN_ID" "$RESULT_DIR/nmi_one_off_query.xml"
query_nmi transaction transaction_id "$SUB_TXN_ID" "$RESULT_DIR/nmi_subscription_txn_query.xml"
query_nmi recurring recurring_id "$PROVIDER_SUB_ID" "$RESULT_DIR/nmi_recurring_query.xml"
assert_query_contains "one-off remote transaction id" "$RESULT_DIR/nmi_one_off_query.xml" "$ONE_OFF_TXN_ID"
ONE_OFF_AMOUNT_DECIMAL="$(printf '%d.%02d' "$((E2E_NMI_ONE_OFF_AMOUNT / 100))" "$((E2E_NMI_ONE_OFF_AMOUNT % 100))")"
assert_query_contains "one-off remote amount" "$RESULT_DIR/nmi_one_off_query.xml" "$ONE_OFF_AMOUNT_DECIMAL"
assert_query_status "one-off remote status" "$RESULT_DIR/nmi_one_off_query.xml"
assert_query_currency_if_present "one-off remote currency" "$RESULT_DIR/nmi_one_off_query.xml"
assert_query_contains "subscription remote transaction id" "$RESULT_DIR/nmi_subscription_txn_query.xml" "$SUB_TXN_ID"
RECURRING_AMOUNT_DECIMAL="$(printf '%d.%02d' "$((E2E_NMI_RECURRING_AMOUNT / 100))" "$((E2E_NMI_RECURRING_AMOUNT % 100))")"
assert_query_contains "subscription remote amount" "$RESULT_DIR/nmi_subscription_txn_query.xml" "$RECURRING_AMOUNT_DECIMAL"
assert_query_status "subscription remote transaction status" "$RESULT_DIR/nmi_subscription_txn_query.xml"
assert_query_currency_if_present "subscription remote transaction currency" "$RESULT_DIR/nmi_subscription_txn_query.xml"
assert_query_contains "recurring remote subscription id" "$RESULT_DIR/nmi_recurring_query.xml" "$PROVIDER_SUB_ID"
assert_query_status "recurring remote status" "$RESULT_DIR/nmi_recurring_query.xml"
echo "PASS NMI Query API matches local transaction/subscription ids, amount, currency when returned, and status"

echo "Verifying signed NMI webhook ingestion and idempotent replay..."
NEXT_CHARGE_DATE="$(date -u -d '+1 day' +%F)"
WEBHOOK_BODY="$(jq -nc --arg event_id "e2e_webhook_${E2E_RUN_ID}" --arg sub "$PROVIDER_SUB_ID" --arg plan "$E2E_NMI_PLAN_ID" --arg amount "$RECURRING_AMOUNT_DECIMAL" --arg next_charge_date "$NEXT_CHARGE_DATE" '{event_id:$event_id,event_type:"recurring.subscription.add",event_body:{subscription_id:$sub,next_charge_date:$next_charge_date,plan:{id:$plan,amount:$amount}}}')"
post_signed_nmi_webhook "$WEBHOOK_BODY" "$RESULT_DIR/webhook_first"
assert_http "signed NMI webhook ingestion" 200 "$RESULT_DIR/webhook_first.code" "$RESULT_DIR/webhook_first.body"
post_signed_nmi_webhook "$WEBHOOK_BODY" "$RESULT_DIR/webhook_replay"
assert_http "signed NMI webhook idempotent replay" 200 "$RESULT_DIR/webhook_replay.code" "$RESULT_DIR/webhook_replay.body"
for _ in $(seq 1 90); do
  SUB_ACTIVATION_CHECK="$(psql_scalar "SELECT concat(count(*), '|', bool_and(status='active')) FROM openrails.subscriptions WHERE id = '$SUB_UUID'::uuid;")"
  if [ "$SUB_ACTIVATION_CHECK" = "1|t" ]; then
    break
  fi
  sleep 1
done
if [ "$SUB_ACTIVATION_CHECK" != "1|t" ]; then
  echo "FAIL signed NMI add-subscription webhook did not activate local subscription: $SUB_ACTIVATION_CHECK" >&2
  exit 1
fi
echo "PASS signed NMI webhook ingestion, idempotent replay, and activation"

echo "Cancelling subscription through OpenRails..."
CANCEL_BODY='{"feedback":"nmi live e2e cancel"}'
curl_json POST "$BASE_URL/v1/me/subscriptions/$SUBSCRIPTION_ID/cancel" "$CANCEL_BODY" "$RESULT_DIR/cancel_subscription"
assert_http_accepted_or_ok "subscription cancel" "$RESULT_DIR/cancel_subscription.code" "$RESULT_DIR/cancel_subscription.body"
for _ in $(seq 1 90); do
  CANCEL_CHECK="$(psql_scalar "SELECT concat(count(*), '|', bool_and(status='cancelled')) FROM openrails.subscriptions WHERE id = '$SUB_UUID'::uuid;")"
  if [ "$CANCEL_CHECK" = "1|t" ]; then
    break
  fi
  sleep 1
done
if [ "$CANCEL_CHECK" != "1|t" ]; then
  echo "FAIL local cancellation check: $CANCEL_CHECK" >&2
  exit 1
fi
query_nmi recurring recurring_id "$PROVIDER_SUB_ID" "$RESULT_DIR/nmi_recurring_after_cancel_query.xml"
if grep -Fq "$PROVIDER_SUB_ID" "$RESULT_DIR/nmi_recurring_after_cancel_query.xml"; then
  assert_query_status "recurring after cancel remote status" "$RESULT_DIR/nmi_recurring_after_cancel_query.xml"
  echo "PASS subscription cancelled locally and remote recurring query still resolves"
else
  echo "PASS subscription cancelled locally and NMI no longer returns the deleted recurring id"
fi

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
      collectjs_payment_token:true,
      saved_payment_method:true,
      payment_method_list_readback:true,
      saved_vault_one_off_charge:true,
      saved_vault_subscription_checkout:true,
      nmi_query:true,
      signed_webhook_ingestion:true,
      signed_webhook_idempotent_replay:true,
      cancellation:true
    },
    provider_refs:{
      one_off_transaction_id:$one_off_transaction_id,
      subscription_id:$provider_subscription_id,
      subscription_transaction_id:$subscription_transaction_id
    },
    local_refs:{subscription_id:$subscription_id}
  }' >"$SUMMARY"

echo "== PASS live NMI lifecycle E2E =="
echo "summary: $SUMMARY"
