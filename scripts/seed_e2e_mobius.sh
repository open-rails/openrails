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
E2E_NMI_MERCHANT_SLUG="${E2E_NMI_MERCHANT_SLUG:-local-stack}"
E2E_NMI_PLAN_ID="${E2E_NMI_PLAN_ID:-openrails_e2e_nmi_daily_999}"
E2E_NMI_ONE_OFF_AMOUNT="${E2E_NMI_ONE_OFF_AMOUNT:-499}"
E2E_NMI_RECURRING_AMOUNT="${E2E_NMI_RECURRING_AMOUNT:-999}"

echo "Seeding local OpenRails DB with E2E product/price..."
docker compose -f "$COMPOSE_FILE" exec -T postgres \
  psql -U admin -d openrails_db \
  -v "merchant_slug=${E2E_NMI_MERCHANT_SLUG}" \
  -v "nmi_plan_id=${E2E_NMI_PLAN_ID}" \
  -v "nmi_recurring_amount=${E2E_NMI_RECURRING_AMOUNT}" \
  -v "nmi_one_off_amount=${E2E_NMI_ONE_OFF_AMOUNT}" \
  -f /dev/stdin <"$ROOT_DIR/scripts/seed_e2e_mobius.sql"
