#!/usr/bin/env bash
# The ordinary validation entry point, shared by local development and CI.
set -euo pipefail
cd "$(dirname "$0")/.."

guards() {
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
}

# These builds embed only web/admin/dist/.gitkeep: Go must build without a
# console build.
build() {
  go build ./...
}

go_tests() {
  # Package tests are guards, contracts and focused regressions; database and
  # provider behavior is covered by the e2e suite in End-to-end.
  go test -vet=all -race -count=1 -cover ./...
}

console() {
  bash scripts/build-admin-console.sh
  pnpm --dir web/admin run lint
  pnpm --dir web/admin exec vitest run --maxWorkers=2
  test -f web/admin/dist/index.html || {
    echo "console: web/admin/dist/index.html missing — the embed gate would prove nothing" >&2
    exit 1
  }
  # A host build: the console plus a host's extension, its "@/" imports and
  # Tailwind classes resolved against the host's own tree.
  host_build="$(mktemp -d)"
  bash scripts/build-admin-console.sh \
    --extensions scripts/testdata/console-host/console/index.ts \
    --extensions-src scripts/testdata/console-host/src "$host_build"
  grep -qs 'Console host fixture' "$host_build"/assets/*.js || {
    echo "console: the host build is missing the extension's page" >&2
    exit 1
  }
  grep -qs '0a1b2c' "$host_build"/assets/*.css || {
    echo "console: the host build is missing the extension's Tailwind classes" >&2
    exit 1
  }
  rm -rf "$host_build"
  # One build serves any admin_console.path (#1127).
  bash scripts/go-test-gate.sh ./web/admin TestBuiltConsoleServesAtAnyPath
  git diff --quiet -- web/admin/dist || {
    echo "console: the build removed web/admin/dist/.gitkeep; go build without a console build would fail" >&2
    exit 1
  }
  go build -o /dev/null ./cmd/openrails
}

checks() {
  guards
  build
  go_tests
  console
}

e2e() {
  : "${OPENRAILS_E2E_DSN:?Set OPENRAILS_E2E_DSN to a disposable PostgreSQL server}"
  bash scripts/e2e.sh
}

case "${1:-all}" in
  checks) checks ;;
  build) build ;;
  go) guards; go_tests ;;
  console) console ;;
  e2e) e2e ;;
  all) checks; e2e ;;
  *) echo "usage: bash scripts/check.sh [checks|build|go|console|e2e|all]" >&2; exit 2 ;;
esac
