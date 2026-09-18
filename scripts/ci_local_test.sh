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

require_step "bash scripts/check.sh checks" "compact checks"
require_step "bash scripts/check.sh e2e" "end-to-end qualification"

echo "ci-local contract tests passed"
