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

if [ -z "${E2E_MOBIUS_PLAN_ID:-}" ]; then
  echo "Missing E2E_MOBIUS_PLAN_ID (the sandbox recurring plan ID)" >&2
  echo "Set it in your .env, e.g.: E2E_MOBIUS_PLAN_ID=12345" >&2
  exit 1
fi

COMPOSE_FILE="${COMPOSE_FILE:-$ROOT_DIR/docker-compose.yaml}"

echo "Seeding local OpenRails DB with E2E product/price..."
docker compose -f "$COMPOSE_FILE" exec -T postgres \
  psql -U admin -d openrails_db \
  -v "mobius_plan_id=${E2E_MOBIUS_PLAN_ID}" \
  -f /dev/stdin <"$ROOT_DIR/scripts/seed_e2e_mobius.sql"
