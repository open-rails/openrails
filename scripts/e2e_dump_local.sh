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

FILTER_BY_RUN="false"
if [ -n "$E2E_RUN_ID" ]; then
  FILTER_BY_RUN="true"
fi

echo "Dumping local billing rows..."
echo "  E2E_RUN_ID:  ${E2E_RUN_ID:-<not set>}"
echo "  E2E_USER_ID: ${E2E_USER_ID:-<not set>}"
echo ""

SQL="\\set ON_ERROR_STOP on
\\pset pager off
\\pset format aligned
\\pset border 2

\\echo '--- checkout_sessions ---'
SELECT id, status, processor, price_id, transaction_id, subscription_id, payment_id, created_at
FROM billing.checkout_sessions
WHERE $(if [ "$FILTER_BY_RUN" = "true" ]; then echo "metadata->>'e2e_run_id' = '${E2E_RUN_ID}'"; else echo "user_id = '${E2E_USER_ID}'"; fi)
ORDER BY created_at DESC
LIMIT 50;

\\echo '--- payment_methods ---'
SELECT id, user_id, processor, vault_id, created_at
FROM billing.payment_methods
WHERE $(if [ "$FILTER_BY_RUN" = "true" ]; then echo "metadata->>'e2e_run_id' = '${E2E_RUN_ID}'"; else echo "user_id = '${E2E_USER_ID}'"; fi)
ORDER BY created_at DESC
LIMIT 50;

\\echo '--- subscriptions ---'
SELECT id, user_id, status, processor, processor_subscription_id, price_id, created_at
FROM billing.subscriptions
WHERE $(if [ "$FILTER_BY_RUN" = "true" ]; then echo "gateway_response->>'e2e_run_id' = '${E2E_RUN_ID}'"; else echo "user_id = '${E2E_USER_ID}'::uuid"; fi)
ORDER BY created_at DESC
LIMIT 50;

\\echo '--- payments ---'
SELECT id, user_id, processor, transaction_id, amount, currency, purchased_at
FROM billing.payments
WHERE $(if [ "$FILTER_BY_RUN" = "true" ]; then echo "metadata->>'e2e_run_id' = '${E2E_RUN_ID}'"; else echo "user_id = '${E2E_USER_ID}'::uuid"; fi)
ORDER BY purchased_at DESC
LIMIT 50;
"

docker compose -f "$COMPOSE_FILE" exec -T postgres \
  psql -U admin -d openrails_db -v ON_ERROR_STOP=1 -c "$SQL"
