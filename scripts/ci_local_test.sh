#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

plan="$(task --dry ci-local 2>&1)"

require_step() {
    pattern="$1"
    label="$2"
    printf '%s\n' "$plan" | grep -Fq -- "$pattern" || {
        echo "ci_local_test: missing $label from task ci-local" >&2
        exit 1
    }
}

require_step "task tooling-test" "tooling regressions"
require_step "bash scripts/check_business_time_test.sh" "business-time regressions"
require_step "bash scripts/check_business_time.sh" "business-time repository guard"
require_step "bash scripts/sql-lint.sh" "SQL lint"
require_step "go mod download" "module download"
require_step "go vet ./..." "default vet"
require_step "go build ./..." "default build"
require_step "go vet -tags integration ./..." "integration-tag vet"
require_step 'for d in examples/*/' "example build loop"
require_step 'go test -count=1 -race ./...' "race tests"
require_step "task lint" "pinned lint"
require_step "bash scripts/scan-injected-code.sh --all --self-test" "injected-code scan"
require_step "MIGRATION_PREFIX_NO_FETCH=1 bash scripts/migration-prefix-collision.sh" "migration-prefix check"
require_step "task sqlc-check" "database-backed sqlc check"

echo "ci-local contract tests passed"
