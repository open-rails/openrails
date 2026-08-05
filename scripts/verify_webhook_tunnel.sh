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

if [ -z "${CLOUDFLARED_PUBLIC_HOSTNAME:-}" ]; then
  echo "Missing CLOUDFLARED_PUBLIC_HOSTNAME (set it in .env or your shell)" >&2
  exit 1
fi

PUBLIC_BASE="https://${CLOUDFLARED_PUBLIC_HOSTNAME}"

echo "Probing: $PUBLIC_BASE/health/live"
curl -fsS "$PUBLIC_BASE/health/live" >/dev/null
echo "  OK"

echo "Probing: $PUBLIC_BASE/health/ready"
curl -fsS "$PUBLIC_BASE/health/ready" >/dev/null
echo "  OK"

echo "Webhook base URL:"
echo "  $PUBLIC_BASE/v1/webhooks/nmi"

