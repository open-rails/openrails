#!/usr/bin/env bash
# The ordinary validation entry point, shared by local development and CI.
set -euo pipefail
cd "$(dirname "$0")/.."

checks() {
  bash scripts/scan-injected-code.sh --all
  unformatted="$(git ls-files -z '*.go' | xargs -0 gofmt -l)"
  if [[ -n "$unformatted" ]]; then
    printf 'Run gofmt on:\n%s\n' "$unformatted" >&2
    exit 1
  fi
  go build ./...
  go vet ./...
  go test -race -count=1 ./...
  bash scripts/build-admin-console.sh cmd/openrails/consoleassets/dist
  pnpm --dir web/admin exec vitest run --maxWorkers=2
  go build -tags console_assets -o /dev/null ./cmd/openrails
}

e2e() {
  : "${OPENRAILS_TEST_DB_DSN:?Set OPENRAILS_TEST_DB_DSN to a disposable PostgreSQL server}"
  : "${OPENRAILS_TEST_REDIS_ADDR:?Set OPENRAILS_TEST_REDIS_ADDR to the test Redis server}"
  if [[ -z "${SQLC_DATABASE_URL:-}" ]]; then
    export SQLC_ADMIN_DATABASE_URL="${SQLC_ADMIN_DATABASE_URL:-$OPENRAILS_TEST_DB_DSN}"
    export SQLC_VET_DB="openrails_check_${BASHPID}"
    trap 'psql "$SQLC_ADMIN_DATABASE_URL" -v ON_ERROR_STOP=1 -qc "DROP DATABASE IF EXISTS $SQLC_VET_DB WITH (FORCE)"' EXIT
    SQLC_DATABASE_URL="$(bash scripts/sqlc-vet-db.sh)"
    export SQLC_DATABASE_URL
  fi
  sqlc_bin="${SQLC_BIN:-$(bash scripts/ci-install-tool.sh sqlc)}"
  "$sqlc_bin" generate
  "$sqlc_bin" vet
  git diff --exit-code -- internal/db/gen
  go run ./scripts/contracts -workflows
}

case "${1:-all}" in
  checks) checks ;;
  e2e) e2e ;;
  all) checks; e2e ;;
  *) echo "usage: bash scripts/check.sh [checks|e2e|all]" >&2; exit 2 ;;
esac
