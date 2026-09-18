#!/usr/bin/env bash
# Run one `go test -run <pattern>` CI gate and fail unless a named top-level
# test actually ran and passed.
#
# `go test` reports success for three different kinds of nothing:
#   *  -run matched no test           -> "ok <pkg> [no tests to run]", exit 0
#   *  build tags excluded every _test.go -> "? <pkg> [no test files]", exit 0
#   *  the test called t.Skip         -> "--- SKIP", exit 0
# A retag, rename or typo therefore turns a gate into a silent no-op (or#1013:
# tagging internal/db/sqlaudit `integration && cgo` left `task sqlc-check`
# running the query auditor's -run pattern against nothing, and passing). This
# wrapper reads the run's own output and fails closed on all three.
#
# Usage: scripts/go-test-gate.sh <package> <run-pattern> [extra go test args...]
set -euo pipefail
cd "$(dirname "$0")/.."

if [ "$#" -lt 2 ]; then
  echo "usage: scripts/go-test-gate.sh <package> <run-pattern> [go test args...]" >&2
  exit 2
fi

pkg=$1
pattern=$2
shift 2

log=$(mktemp "${TMPDIR:-/tmp}/go-test-gate.XXXXXX")
trap 'rm -f "$log"' EXIT

fail() {
  echo "go-test-gate: FAIL ${pkg} -run ${pattern}: $*" >&2
  echo "go-test-gate: this gate tested nothing or did not pass — it is not a green result." >&2
  exit 1
}

# -v is what makes the per-test verdicts ("--- PASS: Name") visible; the set of
# tests selected, their build tags and their pass criteria are unchanged.
status=0
go test -v -count=1 -run "$pattern" "$@" "$pkg" 2>&1 | tee "$log" || status=$?

if grep -qF '[build failed]' "$log"; then
  fail "the package failed to build"
fi
if grep -qF '[no test files]' "$log"; then
  fail "the package has no test files under these build tags — nothing was compiled to run"
fi
if grep -qF '[no tests to run]' "$log" || grep -qF 'warning: no tests to run' "$log"; then
  fail "-run ${pattern} matched no test in ${pkg}"
fi
if grep -qE '^--- SKIP: ' "$log"; then
  fail "the selected test skipped: $(grep -E '^--- SKIP: ' "$log" | tr '\n' ' ')"
fi
if [ "$status" -ne 0 ]; then
  fail "go test exited ${status}"
fi

passed=$(grep -cE '^--- PASS: ' "$log" || true)
if [ "$passed" -eq 0 ]; then
  fail "no top-level test reported PASS"
fi

echo "go-test-gate: OK ${pkg} -run ${pattern} (${passed} top-level test(s) passed)"
