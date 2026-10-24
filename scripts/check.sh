#!/usr/bin/env bash
# Local and CI entrypoint for the selected critical replacement and static checks.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off
export GOMAXPROCS=${GOMAXPROCS:-2}
modules=(. server adapters/authkit examples)
in_modules() {
  local module
  for module in "${modules[@]}"; do (cd "$module" && "$@"); done
}
guards() {
  bash scripts/migration-prefix-collision.sh
  bash scripts/check_business_time.sh
  bash scripts/scan-injected-code.sh --all
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
  browser) browser ;;
  chart) chart ;;
  critical) bash scripts/critical-tests.sh ;;
  all)
    : "${OPENRAILS_CRITICAL_DSN:?Set an owned disposable PostgreSQL URL}"
    guards; build; browser; chart
    bash scripts/critical-tests.sh
    ;;
  *) echo 'usage: scripts/check.sh [guards|build|browser|chart|critical|all]' >&2; exit 2 ;;
esac
