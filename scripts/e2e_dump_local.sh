#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -f "$ROOT_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1090
  source "$ROOT_DIR/.env"
  set +a
fi

require() {
  local name="$1"
  if ! command -v "$name" >/dev/null 2>&1; then
    echo "Missing required command: $name" >&2
    exit 1
  fi
}

require docker

COMPOSE_FILE="${COMPOSE_FILE:-$ROOT_DIR/docker-compose.yaml}"

E2E_RUN_ID="${E2E_RUN_ID:-}"
E2E_USER_ID="${E2E_USER_ID:-}"

if [ -z "$E2E_RUN_ID" ] && [ -z "$E2E_USER_ID" ]; then
  echo "Provide E2E_RUN_ID and/or E2E_USER_ID (set env vars or export from scripts/mint_jwt.sh output)." >&2
  exit 1
fi

echo "Dumping local OpenRails rows..."
echo "  E2E_RUN_ID:  ${E2E_RUN_ID:-<not set>}"
echo "  E2E_USER_ID: ${E2E_USER_ID:-<not set>}"
echo ""

SQL="\\set ON_ERROR_STOP on
\\pset pager off
\\pset format aligned
\\pset border 2

\\echo '--- checkout_sessions ---'
SELECT id, status, rail, price_id, transaction_id, subscription_id, payment_id, created_at
FROM openrails.checkout_sessions
WHERE (NULLIF(:'e2e_run_id', '') IS NOT NULL AND metadata->>'e2e_run_id' = :'e2e_run_id')
   OR (NULLIF(:'e2e_run_id', '') IS NULL AND customer_id IN (
     SELECT id FROM openrails.customers WHERE subject = :'e2e_user_id'
   ))
ORDER BY created_at DESC
LIMIT 50;

\\echo '--- payment_methods ---'
SELECT id, customer_id, rail, rail_customer_ref, rail_method_ref, created_at
FROM openrails.payment_methods
WHERE (NULLIF(:'e2e_run_id', '') IS NOT NULL AND metadata->>'e2e_run_id' = :'e2e_run_id')
   OR (NULLIF(:'e2e_run_id', '') IS NULL AND customer_id IN (
     SELECT id FROM openrails.customers WHERE subject = :'e2e_user_id'
   ))
ORDER BY created_at DESC
LIMIT 50;

\\echo '--- subscriptions ---'
SELECT id, customer_id, status, rail, rail_subscription_id, price_id, created_at
FROM openrails.subscriptions
WHERE (NULLIF(:'e2e_run_id', '') IS NOT NULL AND gateway_response->>'e2e_run_id' = :'e2e_run_id')
   OR (NULLIF(:'e2e_run_id', '') IS NULL AND customer_id IN (
     SELECT id FROM openrails.customers WHERE subject = :'e2e_user_id'
   ))
ORDER BY created_at DESC
LIMIT 50;

\\echo '--- payments ---'
SELECT id, customer_id, rail, transaction_id, amount, currency, purchased_at
FROM openrails.payments
WHERE (NULLIF(:'e2e_run_id', '') IS NOT NULL AND metadata->>'e2e_run_id' = :'e2e_run_id')
   OR (NULLIF(:'e2e_run_id', '') IS NULL AND customer_id IN (
     SELECT id FROM openrails.customers WHERE subject = :'e2e_user_id'
   ))
ORDER BY purchased_at DESC
LIMIT 50;
"

docker compose -f "$COMPOSE_FILE" exec -T postgres \
  psql -U admin -d openrails_db -v ON_ERROR_STOP=1 \
  -v e2e_run_id="$E2E_RUN_ID" -v e2e_user_id="$E2E_USER_ID" <<<"$SQL"
