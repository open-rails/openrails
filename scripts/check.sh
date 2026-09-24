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
  bash scripts/scan-injected-code.sh --all
  go_files=()
  while IFS= read -r -d '' file; do
    [[ -e "$file" ]] && go_files+=("$file")
  done < <(git ls-files -z '*.go')
  unformatted="$(printf '%s\0' "${go_files[@]}" | xargs -0 --no-run-if-empty gofmt -l)"
  if [[ -n "$unformatted" ]]; then
    printf 'Run gofmt on:\n%s\n' "$unformatted" >&2
    exit 1
  fi
  bash scripts/check-embedded-auth-boundary.sh
  go build ./...
  # Package tests are guards, contracts and focused regressions; database and
  # provider behavior is covered by the greenfield suite in End-to-end.
  go test -vet=all -race -count=1 -cover ./...
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
  : "${OPENRAILS_GREENFIELD_DSN:?Set OPENRAILS_GREENFIELD_DSN to a disposable PostgreSQL server}"
  bash scripts/greenfield.sh
}

case "${1:-all}" in
  checks) checks ;;
  e2e) e2e ;;
  all) checks; e2e ;;
  *) echo "usage: bash scripts/check.sh [checks|e2e|all]" >&2; exit 2 ;;
esac
