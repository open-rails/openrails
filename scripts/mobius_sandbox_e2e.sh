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
OPENRAILS_LOCAL_URL="${OPENRAILS_LOCAL_URL:-${BILLING_LOCAL_URL:-http://localhost:3053}}"
COMPOSE_PROFILES="${COMPOSE_PROFILES:-all}"

require() {
  local name="$1"
  if ! command -v "$name" >/dev/null 2>&1; then
    echo "Missing required command: $name" >&2
    exit 1
  fi
}

require curl
require openssl
require docker
require cloudflared

if [ -z "${CLOUDFLARED_TUNNEL_TOKEN:-}" ]; then
  echo "Missing CLOUDFLARED_TUNNEL_TOKEN" >&2
  exit 1
fi
if [ -z "${CLOUDFLARED_PUBLIC_HOSTNAME:-}" ]; then
  echo "Missing CLOUDFLARED_PUBLIC_HOSTNAME" >&2
  exit 1
fi
MOBIUS_WEBHOOK_SIGNING_SECRET="${MOBIUS_WEBHOOK_SIGNING_SECRET:-${MOBIUS_WEBHOOK_SECRET:-}}"
if [ -z "$MOBIUS_WEBHOOK_SIGNING_SECRET" ]; then
  echo "Missing MOBIUS_WEBHOOK_SIGNING_SECRET (OpenRails will reject unsigned webhooks)" >&2
  exit 1
fi

echo "1) Starting docker compose stack..."
PROFILE_ARGS=()
IFS=',' read -r -a PROFILES <<<"$COMPOSE_PROFILES"
for p in "${PROFILES[@]}"; do
  p="$(echo "$p" | xargs)"
  if [ -n "$p" ]; then
    PROFILE_ARGS+=(--profile "$p")
  fi
done
docker compose -f "$COMPOSE_FILE" "${PROFILE_ARGS[@]}" up -d

echo "2) Waiting for OpenRails health..."
for i in {1..60}; do
  if curl -fsS "$OPENRAILS_LOCAL_URL/health/live" >/dev/null 2>&1; then
    echo "  OpenRails is up: $OPENRAILS_LOCAL_URL"
    break
  fi
  sleep 1
  if [ "$i" -eq 60 ]; then
    echo "OpenRails did not become healthy in time: $OPENRAILS_LOCAL_URL/health/live" >&2
    exit 1
  fi
done

echo "3) Starting cloudflared tunnel in background..."
cloudflared tunnel run --token "$CLOUDFLARED_TUNNEL_TOKEN" >/tmp/cloudflared-openrails-webhooks.log 2>&1 &
TUNNEL_PID="$!"

cleanup() {
  echo ""
  echo "Stopping cloudflared (pid=$TUNNEL_PID)..."
  kill "$TUNNEL_PID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

PUBLIC_BASE="https://${CLOUDFLARED_PUBLIC_HOSTNAME}"

echo "4) Probing public hostname routing..."
curl -fsS "$PUBLIC_BASE/health/live" >/dev/null
echo "  public routing OK: $PUBLIC_BASE"

echo "5) Sending signed test webhook to public URL..."
EVENT_ID="e2e_test_$(date +%s%N)"
BODY="{\"event_id\":\"$EVENT_ID\",\"event_type\":\"test\",\"event_body\":{}}"
TS="$(date +%s)"
SIG="$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$MOBIUS_WEBHOOK_SIGNING_SECRET" | awk '{print $NF}')"

curl -i "$PUBLIC_BASE/v1/webhooks/mobius" \
  -H "Content-Type: application/json" \
  -H "X-Signature: t=$TS,s=$SIG" \
  --data "$BODY"

echo ""
echo "Next steps:"
echo "  - Register webhook URL in Mobius/NMI portal:"
echo "      $PUBLIC_BASE/v1/webhooks/mobius"
echo "  - Use tokenization harness (if env=dev):"
echo "      $PUBLIC_BASE/debug/nmi/tokenization?mode=real&provider=mobius"
echo ""
echo "Cloudflared logs: /tmp/cloudflared-openrails-webhooks.log"
echo "Leave this running while testing. Ctrl+C to stop."

wait "$TUNNEL_PID"
