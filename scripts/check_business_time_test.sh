#!/usr/bin/env bash
set -euo pipefail
checker="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check_business_time.sh"
fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT
mkdir -p "${fixture}"/{scripts,internal/modules,internal/river,internal/http/handlers,pkg/service}
printf '# test allowlist\n' >"${fixture}/scripts/business-time-allowlist.txt"
bash "${checker}" "${fixture}" >"${fixture}/result.log" 2>&1 || { cat "${fixture}/result.log" >&2; exit 1; }

expect_failure() {
  if bash "${checker}" "${fixture}" >"${fixture}/result.log" 2>&1; then
    echo "clock guard incorrectly accepted $1" >&2; exit 1
  fi
  grep -q "$2" "${fixture}/result.log" || { cat "${fixture}/result.log" >&2; exit 1; }
}
cat >"${fixture}/internal/modules/policy.go" <<'GO'
package fixture
func expires() { expiry := time.Now().Add(24 * time.Hour) }
GO
expect_failure 'a new business clock' 'unclassified business-time usage'
cat >"${fixture}/internal/modules/policy.go" <<'GO'
package fixture
func cache() {
now := time.Now()
}
GO
printf '%s\n' 'internal/modules/policy.go|now := time.Now()|infrastructure_time|test cache timing|1' >"${fixture}/scripts/business-time-allowlist.txt"
bash "${checker}" "${fixture}" >"${fixture}/result.log" 2>&1 || { cat "${fixture}/result.log" >&2; exit 1; }
cat >>"${fixture}/internal/modules/policy.go" <<'GO'
func businessPolicy() {
now := time.Now()
}
GO
expect_failure 'an extra identical call in an allowed file' 'occurrence changed'
printf '%s\n' 'internal/modules/missing.go|now := time.Now()|infrastructure_time|test missing path|1' >"${fixture}/scripts/business-time-allowlist.txt"
expect_failure 'a retired allowlist path' 'stale clock allowlist path'
printf '%s\n' 'internal/modules/policy.go|now := time.Now()|unreviewed|not a classification|2' >"${fixture}/scripts/business-time-allowlist.txt"
expect_failure 'an invalid classification' 'invalid clock classification'
printf '# empty\n' >"${fixture}/scripts/business-time-allowlist.txt"
rmdir "${fixture}/internal/river"
expect_failure 'a missing scan directory' 'missing scan directory'
echo 'business-time guardrail regression tests passed'
