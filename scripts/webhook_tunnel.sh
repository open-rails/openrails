#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -f "$ROOT_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1090
  source "$ROOT_DIR/.env"
  set +a
fi

if ! command -v cloudflared >/dev/null 2>&1; then
  echo "cloudflared is required but not found on PATH" >&2
  exit 1
fi

if [ -z "${CLOUDFLARED_TUNNEL_TOKEN:-}" ]; then
  echo "Missing CLOUDFLARED_TUNNEL_TOKEN (set it in .env or your shell)" >&2
  exit 1
fi

echo "Starting cloudflared tunnel..."
if [ -n "${CLOUDFLARED_TUNNEL_NAME:-}" ]; then
  echo "  tunnel: ${CLOUDFLARED_TUNNEL_NAME}"
fi
if [ -n "${CLOUDFLARED_PUBLIC_HOSTNAME:-}" ]; then
  echo "  public hostname: https://${CLOUDFLARED_PUBLIC_HOSTNAME}"
  echo "  webhook URL: https://${CLOUDFLARED_PUBLIC_HOSTNAME}/v1/webhooks/nmi"
fi

exec cloudflared tunnel run --token "$CLOUDFLARED_TUNNEL_TOKEN"
