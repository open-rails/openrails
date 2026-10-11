#!/usr/bin/env bash
# The ordinary validation entry point, shared by local development and CI.
set -euo pipefail
cd "$(dirname "$0")/.."
# Each module is proven on its own, never through a developer's go.work.
export GOWORK=off
# The root library, the server released with it, and the examples.
modules=(. server examples)

# in_modules runs a command in every module.
in_modules() {
  local module
  for module in "${modules[@]}"; do
    (cd "$module" && "$@")
  done
}

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

# These builds embed only server/web/console/dist/.gitkeep: Go must build without a
# console build.
build() {
  in_modules go build -o /dev/null ./...
}

go_tests() {
  # Package tests are guards, contracts and focused regressions; database and
  # provider behavior is covered by the e2e suite in End-to-end.
  in_modules go test -vet=all -race -count=1 -cover ./...
}

console() {
  bash scripts/build-admin-console.sh
  pnpm --dir server/web/console run lint
  pnpm --dir server/web/console exec vitest run --maxWorkers=2
  test -f server/web/console/dist/index.html || {
    echo "console: server/web/console/dist/index.html missing — the embed gate would prove nothing" >&2
    exit 1
  }
  # One build serves any admin_console.path (#1127).
  bash scripts/go-test-gate.sh -C server ./internal/adminconsole TestBuiltConsoleServesAtAnyPath
  git diff --quiet -- server/web/console/dist || {
    echo "console: the build removed server/web/console/dist/.gitkeep; go build without a console build would fail" >&2
    exit 1
  }
  (cd server && go build -o /dev/null ./cmd/openrails)
  bash scripts/build-checkout-page.sh
  test -f web/checkout/dist/index.html || {
    echo "checkout: web/checkout/dist/index.html missing" >&2
    exit 1
  }
  git diff --quiet -- web/checkout/dist || {
    echo "checkout: the build removed web/checkout/dist/.gitkeep; go build without a page build would fail" >&2
    exit 1
  }
}

# The Helm chart, with its defaults and with every optional part on.
chart() {
  local helm="${HELM:-helm}" dir=deploy/helm/openrails values
  for values in "$dir/values.yaml" "$dir/ci/full-values.yaml"; do
    "$helm" lint --strict "$dir" -f "$values"
    "$helm" template openrails "$dir" -f "$values" >/dev/null
  done
}

checks() {
  guards
  build
  go_tests
  console
  chart
}

e2e() {
  : "${OPENRAILS_E2E_DSN:?Set OPENRAILS_E2E_DSN to a disposable PostgreSQL server}"
  bash scripts/e2e.sh
  examples_e2e
}

# The examples' end-to-end tests: own-auth's, and standalone's against the
# server built from this checkout.
examples_e2e() {
  : "${OPENRAILS_E2E_DSN:?Set OPENRAILS_E2E_DSN to a disposable PostgreSQL server}"
  local dir
  dir="$(mktemp -d)"
  # shellcheck disable=SC2064 # the directory is fixed now
  trap "rm -rf '$dir'" EXIT
  (cd server && go build -o "$dir/openrails" ./cmd/openrails)
  (cd examples && OPENRAILS_BIN="$dir/openrails" go test -count=1 -tags e2e,integration ./own-auth/ ./standalone/)
}

case "${1:-all}" in
  checks) checks ;;
  build) build ;;
  go) guards; go_tests ;;
  console) console ;;
  chart) chart ;;
  e2e) e2e ;;
  examples-e2e) examples_e2e ;;
  all) checks; e2e ;;
  *) echo "usage: bash scripts/check.sh [checks|build|go|console|chart|e2e|examples-e2e|all]" >&2; exit 2 ;;
esac
