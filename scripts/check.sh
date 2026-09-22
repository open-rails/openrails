#!/usr/bin/env bash
# The ordinary validation entry point, shared by local development and CI.
set -euo pipefail
cd "$(dirname "$0")/.."

checks() {
  # Keep the source-level safety checks in the compact entrypoint. These are
  # cheap and catch regressions that a build or an end-to-end journey cannot
  # observe (business-time injection, raw SQL, and migration lock hazards).
  bash scripts/migration-prefix-collision.sh
  bash scripts/check_business_time_test.sh
  bash scripts/check_business_time.sh
  bash scripts/go-test-gate_test.sh
  bash scripts/test_integration_test.sh
  bash scripts/scan-injected-code.sh --all
  unformatted="$(git ls-files -z '*.go' | xargs -0 gofmt -l)"
  if [[ -n "$unformatted" ]]; then
    printf 'Run gofmt on:\n%s\n' "$unformatted" >&2
    exit 1
  fi
  go build ./...
  go vet ./...
  # Integration-tagged files are a separate compilation universe; keep the
  # compile/vet guard that catches dependency drift before the E2E runner.
  go vet -tags=integration ./...
  # Integration packages run once, with race coverage, in E2E. Retain whole
  # packages here when E2E tags exclude a default source or test file.
  local selected package
  local -a unit_packages=()
  selected="$(bash scripts/test_integration.sh --list-checks-packages)"
  while IFS= read -r package; do
    [[ -n "$package" ]] && unit_packages+=("$package")
  done <<< "$selected"
  go test -race -count=1 "${unit_packages[@]}"
  bash scripts/build-admin-console.sh cmd/openrails/consoleassets/dist
  pnpm --dir web/admin run lint
  pnpm --dir web/admin exec vitest run --maxWorkers=2
  test -n "$(ls -A cmd/openrails/consoleassets/dist 2>/dev/null)" || {
    echo "console: dist/ is empty — the embed gate would prove nothing" >&2
    exit 1
  }
  go build -tags console_assets -o /dev/null ./cmd/openrails
  go vet -tags console_assets ./cmd/openrails/consoleassets
  stray="$(grep -rlE '^//go:build .*console_assets' --include='*.go' . |
    grep -v '^\./cmd/openrails/consoleassets/' || true)"
  if [[ -n "$stray" ]]; then
    echo "console_assets-conditional source outside cmd/openrails/consoleassets:" >&2
    echo "$stray" >&2
    exit 1
  fi
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
  CGO_ENABLED=1 bash scripts/go-test-gate.sh ./internal/db/sqlaudit/ '^TestQueryAudit$'
  bash scripts/sql-lint.sh
  bash scripts/migration-lint.sh
  go run ./scripts/contracts -workflows
}

case "${1:-all}" in
  checks) checks ;;
  e2e) e2e ;;
  all) checks; e2e ;;
  *) echo "usage: bash scripts/check.sh [checks|e2e|all]" >&2; exit 2 ;;
esac
