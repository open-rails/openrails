#!/usr/bin/env bash
# Local and CI entrypoint for the selected critical replacement and static checks.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off
export GOMAXPROCS=${GOMAXPROCS:-2}
modules=()
while IFS= read -r mod; do modules+=("$(dirname "$mod")"); done < <(git ls-files go.mod '*/go.mod')
in_modules() {
  local module
  for module in "${modules[@]}"; do (cd "$module" && "$@"); done
}
# Static guards: one command each.
guards() {
  go run ./scripts/business-time
  bash scripts/check-one-pull-entry.sh
  bash scripts/check-borrowed-connections.sh
  bash scripts/check-logging.sh
  bash scripts/scan-injected-code.sh --all
  go run ./scripts/contracts -lint
  go run ./scripts/docscheck
  gofmt_clean
}
gofmt_clean() {
  local files=() unformatted
  while IFS= read -r -d '' file; do
    [[ -e "$file" ]] && files+=("$file")
  done < <(git ls-files -z '*.go')
  unformatted="$(printf '%s\0' "${files[@]}" | xargs -0 --no-run-if-empty gofmt -l)"
  if [[ -n "$unformatted" ]]; then
    printf 'Run gofmt on:\n%s\n' "$unformatted" >&2
    return 1
  fi
}
build() {
  in_modules go build -o /dev/null ./...
  bash scripts/check-embedded-auth-boundary.sh
}
# Every build tag in the repository at once; no constraint negates one, so
# this run sees every file.
vet() {
  local tags
  if git grep -n '^//go:build.*!' -- '*.go'; then
    echo 'A negated build constraint hides files from the tagged vet.' >&2
    return 1
  fi
  tags="$(git grep -h '^//go:build' -- '*.go' | cut -c11- | grep -oE '[[:alnum:]_.]+' | sort -u | paste -sd, -)"
  in_modules go vet -tags "$tags" ./...
}
# Package tests need nothing outside their process. A test that needs
# PostgreSQL, Redis, Vault or a fixture process is tagged integration.
unit() {
  local module packages
  for module in "${modules[@]}"; do
    # A module of only tagged tests (critical/server-fixture) has none.
    packages="$(cd "$module" && go list ./...)"
    [[ -z "$packages" ]] || (cd "$module" && go test ./...)
  done
}
browser() {
  # The SDK and both consuming apps share this one SDK build. E2E behavior
  # runs only in critical/public_sdk_test.go against its real service fixture.
  pnpm --dir sdk/billing-ui install --frozen-lockfile --ignore-scripts
  pnpm --dir sdk/billing-ui critical:build
  pnpm --dir sdk/billing-ui critical:static
  pnpm --dir sdk/billing-ui contract:check
  local project
  for project in server/web/console web/pay; do
    pnpm --dir "$project" install --frozen-lockfile --ignore-scripts
    pnpm --dir "$project" build
    test -f "$project/dist/index.html"
    git diff --exit-code -- "$project/dist/.gitkeep"
  done
  pnpm --dir server/web/console lint
}
chart() {
  local helm="${HELM:-helm}" dir=deploy/helm/openrails values
  for values in "$dir/values.yaml" "$dir/ci/full-values.yaml"; do
    "$helm" lint --strict "$dir" -f "$values"
    "$helm" template openrails "$dir" -f "$values" >/dev/null
  done
}
case "${1:-all}" in
  guards) guards ;;
  build) build ;;
  vet) vet ;;
  unit) unit ;;
  browser) browser ;;
  chart) chart ;;
  critical) bash scripts/critical-tests.sh ;;
  all)
    : "${OPENRAILS_CRITICAL_DSN:?Set an owned disposable PostgreSQL URL}"
    guards; build; vet; unit; browser; chart
    bash scripts/critical-tests.sh
    ;;
  *) echo 'usage: scripts/check.sh [guards|build|vet|unit|browser|chart|critical|all]' >&2; exit 2 ;;
esac
