#!/usr/bin/env bash
# One-shot provisioning of a LOCAL OpenRails stack for this demo, against the
# real NMI sandbox. Proven end-to-end 2026-07-28 (see ../E2E.md, tracker #825).
#
# Requires in the repo root .env (never printed):
#   NMI_SANDBOX_SECURITY_KEY  NMI_TOKENIZATION_KEY  [NMI_WEBHOOK_SIGNING_SECRET]
#
# What it does (and why it deviates from the README's MODE-1 story — the
# shipped `openrails server` never loads /etc/openrails/merchants.yaml at boot,
# see E2E.md "Engine bugs"):
#   1. builds the demo + mintadmin, generates the issuer keypair
#   2. writes throwaway merchants/catalog manifests + a compose override
#      (PROVIDER_WRITE_MODE=full, MERCHANT_SOURCE=api, /etc/openrails mount)
#   3. docker compose up; push-merchant-config CLI (manifest mode) creates the
#      merchant + issuer-as-owner + PSP row; catalog push creates the product
#   4. seeds NMI credentials over PUT /v1/merchant/payment-providers/nmi
#      (admin auth = delegated JWT from the issuer key, permissions merchant:*)
#   5. creates a randomized-amount NMI sandbox plan (dup-transaction dodge) and
#      version-bumps the price onto it via the catalog API
#   6. sets merchants.api_host='localhost' (psql; no API exists) so the public
#      catalog (/v1/products) resolves the merchant from Host
#   7. mints the backend API key -> $STATE/apikey.env
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
DEMO=$(cd "$HERE/.." && pwd)
REPO=$(cd "$DEMO/../.." && pwd)
STATE=${STATE:-"$HERE/.state"}
mkdir -p "$STATE/etc-openrails"

set -a; source "$REPO/.env"; set +a
: "${NMI_SANDBOX_SECURITY_KEY:?set in $REPO/.env}"
: "${NMI_TOKENIZATION_KEY:?set in $REPO/.env}"
NMI_TOKENIZATION_URL=${NMI_TOKENIZATION_URL:-https://secure.networkmerchants.com/token/Collect.js}
NMI_WEBHOOK_SIGNING_SECRET=${NMI_WEBHOOK_SIGNING_SECRET:-demo-webhook-secret}
NMI_ACCOUNT_ID=${NMI_ACCOUNT_ID:-579145} # operator-declared label (NMI Gateway ID)
BASE=${OPENRAILS_BASE_URL:-http://localhost:3053}

echo "== build demo + helpers"
(cd "$DEMO" && go build . && go build -o "$STATE/mintadmin" ./e2e/mintadmin)

echo "== issuer keypair"
if [ ! -f "$STATE/issuer_key.pem" ]; then
  (cd "$STATE" && ISSUER_KEY_FILE="$STATE/issuer_key.pem" OPENRAILS_API_KEY=placeholder \
    "$DEMO/gated-premium-page" >/dev/null 2>&1 || true)
  [ -f "$STATE/issuer_key.pem" ] || { echo "issuer key generation failed"; exit 1; }
fi

echo "== manifests + compose override"
PUB_INDENTED=$(sed 's/^/            /' "$STATE/issuer_key.pem.pub")
cat > "$STATE/etc-openrails/merchants.yaml" <<EOF
version: 1
merchants:
  demo:
    display_name: Demo
    remote_application:
      issuer: https://gated-premium-page.example
      public_keys:
        - kid: demo-1
          public_key_pem: |
$PUB_INDENTED
    profile:
      display_name: Demo Billing
      from_email: billing@demo.example
    psps:
      mobius-sandbox:
        nmi:
          environment: test
          account_id: "$NMI_ACCOUNT_ID"
          settings:
            tokenization_url: $NMI_TOKENIZATION_URL
            tokenization_key: $NMI_TOKENIZATION_KEY
          secrets:
            security_key: $NMI_SANDBOX_SECURITY_KEY
            webhook_signing_secret: $NMI_WEBHOOK_SIGNING_SECRET
EOF
cat > "$STATE/etc-openrails/catalog.yaml" <<'EOF'
version: 1
catalogs:
  - merchant: demo
    products:
      - key: premium
        display_name: Premium
        description: Premium access subscription.
        entitlements: [premium]
        prices:
          - currency: usd
            unit_amount: 5_230_000
            duration: 30d
            auto_renew: true
            psps: [mobius-sandbox]   # NMI is link-only: the plan link is added in step 5
EOF
cat > "$STATE/override.yaml" <<EOF
services:
  openrails:
    environment:
      PROVIDER_WRITE_MODE: "full"   # unset fail-closes to readonly; sandbox account, no real money
      MERCHANT_SOURCE: "api"        # engine gap: 'server' never loads the MODE-1 boot manifest
    volumes:
      - $STATE/etc-openrails:/etc/openrails:ro
EOF

echo "== stack up"
(cd "$REPO" && nice -n 10 docker compose -f docker-compose.yaml -f "$STATE/override.yaml" --profile all up -d)
for i in $(seq 1 60); do curl -sf -m 2 "$BASE/health/ready" >/dev/null && break; sleep 2; done
curl -sf -m 2 "$BASE/health/ready" >/dev/null || { echo "stack not ready"; exit 1; }

echo "== push merchant + catalog (CLI runs in manifest mode; server stays api mode)"
docker compose -f "$REPO/docker-compose.yaml" exec -T -e MERCHANT_SOURCE=manifest openrails \
  openrails push-merchant-config -f /etc/openrails/merchants.yaml --insert --overwrite >/dev/null
docker compose -f "$REPO/docker-compose.yaml" exec -T -e MERCHANT_SOURCE=manifest openrails \
  openrails push-merchant-catalog -f /etc/openrails/catalog.yaml --insert --overwrite >/dev/null

# The server's delegated-token verifier caches registered issuer keys in
# process memory; a key (re)registered by the CLI is only picked up on restart.
(cd "$REPO" && docker compose -f docker-compose.yaml restart openrails >/dev/null 2>&1)
for i in $(seq 1 30); do curl -sf -m 2 "$BASE/health/ready" >/dev/null && break; sleep 2; done

ADMIN_JWT=$("$STATE/mintadmin" -key "$STATE/issuer_key.pem" -perms 'merchant:*')

echo "== seed NMI credentials via merchant API (MODE 2 store)"
jq -n --arg sk "$NMI_SANDBOX_SECURITY_KEY" --arg ws "$NMI_WEBHOOK_SIGNING_SECRET" \
      --arg tk "$NMI_TOKENIZATION_KEY" --arg tu "$NMI_TOKENIZATION_URL" --arg id "$NMI_ACCOUNT_ID" \
  '{environment:"test",account_id:$id,public_config:{tokenization_key:$tk,tokenization_url:$tu},credentials:{security_key:$sk,webhook_signing_secret:$ws}}' |
curl -sf -X PUT "$BASE/v1/merchant/payment-providers/nmi" \
  -H "Authorization: Bearer $ADMIN_JWT" -H 'Content-Type: application/json' --data @- >/dev/null

echo "== NMI plan (randomized amount vs the duplicate-transaction window) + price link"
CENTS=$((500 + RANDOM % 500))
AMT="$((CENTS / 100)).$(printf %02d $((CENTS % 100)))"
PLAN="openrails_demo_premium_${CENTS}_d30"
RESP=$(curl -s https://secure.networkmerchants.com/api/transact.php \
  --data-urlencode "security_key=$NMI_SANDBOX_SECURITY_KEY" \
  -d "recurring=add_plan&plan_id=$PLAN&plan_name=OpenRails Demo Premium&plan_amount=$AMT&day_frequency=30&plan_payments=0")
case "$RESP" in *response=1*|*exist*|*already*|*uplicate*) ;; *) echo "add_plan failed"; exit 1;; esac
PRODUCT_ID=$(curl -sf "$BASE/v1/products" | jq -r '.data[] | select(.key=="premium").id' | sed 's/^prod_//')
# Same key + different amount = version bump: old substance archives, checkout
# sells the fresh amount (Attach verifies the plan at NMI before accepting).
curl -sf -X POST "$BASE/v1/merchant/catalog/prices" \
  -H "Authorization: Bearer $ADMIN_JWT" -H 'Content-Type: application/json' \
  -d "{\"product_id\":\"$PRODUCT_ID\",\"key\":\"premium-monthly\",\"unit_amount\":$((CENTS * 10000)),\"currency\":\"usd\",\"access_duration_hours\":720,\"auto_renew\":true,\"psp_links\":{\"mobius-sandbox\":{\"rail\":\"nmi\",\"plan_id\":\"$PLAN\"}}}" >/dev/null

echo "== api_host (no API surface exists; same row update SetHostConfig performs)"
docker compose -f "$REPO/docker-compose.yaml" exec -T postgres \
  psql -U admin -d openrails_db -qc "UPDATE openrails.merchants SET api_host='localhost' WHERE slug='demo';"

echo "== mint backend API key"
KEY=$(curl -sf -X POST "$BASE/v1/merchant/api-keys" \
  -H "Authorization: Bearer $ADMIN_JWT" -H 'Content-Type: application/json' \
  -d '{"name":"demo-e2e","role":"owner"}' | jq -r .secret)
umask 077; echo "OPENRAILS_API_KEY=$KEY" > "$STATE/apikey.env"

echo "provisioned. Run the demo:"
echo "  set -a; source $REPO/.env; source $STATE/apikey.env; set +a"
echo "  ISSUER_KEY_FILE=$STATE/issuer_key.pem $DEMO/gated-premium-page"
echo "or the headless proof: e2e/run-e2e.sh"
