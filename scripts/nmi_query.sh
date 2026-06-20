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

SECURITY_KEY="${NMI_QUERY_SECURITY_KEY:-}"
if [ -z "$SECURITY_KEY" ]; then
  echo "Missing NMI_QUERY_SECURITY_KEY" >&2
  exit 1
fi

TXN_ID=""
SUB_ID=""
while [ $# -gt 0 ]; do
  case "$1" in
    --transaction-id)
      TXN_ID="${2:-}"
      shift 2
      ;;
    --subscription-id|--recurring-id)
      SUB_ID="${2:-}"
      shift 2
      ;;
    *)
      echo "Unknown arg: $1" >&2
      echo "Usage: $0 [--transaction-id <id>] [--subscription-id <id>]" >&2
      exit 1
      ;;
  esac
done

if [ -z "$TXN_ID" ] && [ -z "$SUB_ID" ]; then
  echo "Provide --transaction-id and/or --subscription-id" >&2
  exit 1
fi

QUERY_URL="https://secure.nmi.com/api/query.php"

echo "Query URL: $QUERY_URL"

if [ -n "$TXN_ID" ]; then
  echo ""
  echo "== transaction query =="
  curl -fsS -X POST "$QUERY_URL" \
    -d "security_key=$SECURITY_KEY" \
    -d "report_type=transaction" \
    -d "transaction_id=$TXN_ID"
  echo ""
fi

if [ -n "$SUB_ID" ]; then
  echo ""
  echo "== recurring query =="
  curl -fsS -X POST "$QUERY_URL" \
    -d "security_key=$SECURITY_KEY" \
    -d "report_type=recurring" \
    -d "recurring_id=$SUB_ID"
  echo ""
fi
