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

require curl

new_uuid() {
  if [ -r /proc/sys/kernel/random/uuid ]; then
    cat /proc/sys/kernel/random/uuid
    return
  fi
  # Fallback: timestamp-based pseudo-UUID (not ideal, but avoids extra deps)
  date +%s%N
}

AUD="${AUTHKIT_AUDIENCE:-openrails-app}"
MINT_URL="${AUTHKIT_MINT_URL:-http://localhost:28080/api/v1/dev/mint}"

if [ -z "${AUTHKIT_DEV_MINT_SECRET:-}" ]; then
  echo "Missing AUTHKIT_DEV_MINT_SECRET (set it in .env or your shell)" >&2
  exit 1
fi

E2E_RUN_ID="${E2E_RUN_ID:-e2e_$(date +%Y%m%dT%H%M%S)_$(new_uuid)}"
E2E_USER_ID="${E2E_USER_ID:-$(new_uuid)}"

EMAIL="${E2E_EMAIL:-e2e+${E2E_RUN_ID}@example.com}"

BODY="$(cat <<JSON
{"sub":"$E2E_USER_ID","aud":"$AUD","email":"$EMAIL","expires_in_seconds":3600}
JSON
)"

echo "Minting JWT..."
echo "  mint_url: $MINT_URL"
echo "  aud:      $AUD"
echo "  sub:      $E2E_USER_ID"
echo "  run_id:   $E2E_RUN_ID"

RAW="$(curl -fsS "$MINT_URL" \
  -H "Authorization: Bearer $AUTHKIT_DEV_MINT_SECRET" \
  -H "Content-Type: application/json" \
  --data "$BODY")"

TOKEN="$(printf '%s' "$RAW" | sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\\([^"]*\\)".*/\\1/p')"
if [ -z "$TOKEN" ]; then
  echo "Failed to parse token from response:" >&2
  echo "$RAW" >&2
  exit 1
fi

echo ""
echo "Use these for E2E calls:"
echo "  export E2E_RUN_ID=\"$E2E_RUN_ID\""
echo "  export E2E_USER_ID=\"$E2E_USER_ID\""
echo "  export E2E_JWT=\"$TOKEN\""
echo ""
echo "Example header:"
echo "  Authorization: Bearer \$E2E_JWT"
echo "  X-E2E-Run-ID: \$E2E_RUN_ID"
echo "  X-Idempotency-Key: e2e_\${E2E_RUN_ID}_checkout"
