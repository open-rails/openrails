#!/usr/bin/env bash
# Regression test for scripts/go-test-gate.sh: the guard itself must fail when
# a gate would have tested nothing, and must stay out of the way when the gate
# really ran. Fixtures live in scripts/testdata/gatefixture*.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

gate="scripts/go-test-gate.sh"
pkg="./scripts/testdata/gatefixture"
excluded="./scripts/testdata/gatefixture-excluded"
out=$(mktemp)
trap 'rm -f "${out}"' EXIT

expect_pass() {
  local name=$1
  shift
  if ! bash "${gate}" "$@" >"${out}" 2>&1; then
    echo "go-test-gate self-test: ${name}: expected the gate to pass, it failed:" >&2
    cat "${out}" >&2
    exit 1
  fi
}

expect_fail() {
  local name=$1 needle=$2
  shift 2
  if bash "${gate}" "$@" >"${out}" 2>&1; then
    echo "go-test-gate self-test: ${name}: expected the gate to FAIL, it passed:" >&2
    cat "${out}" >&2
    exit 1
  fi
  if ! grep -qF "${needle}" "${out}"; then
    echo "go-test-gate self-test: ${name}: expected the failure to mention '${needle}':" >&2
    cat "${out}" >&2
    exit 1
  fi
}

expect_pass  "a real test"        "${pkg}" TestGateFixturePasses
expect_pass  "several matches"    "${pkg}" 'TestGateFixturePasses|TestGateFixturePassesToo'
expect_fail  "pattern matches nothing" "matched no test" \
             "${pkg}" TestGateFixtureNoSuchTestExists
expect_fail  "selected test skips"     "skipped" \
             "${pkg}" TestGateFixtureSkips
expect_fail  "real failure"            "exited" \
             "${pkg}" TestGateFixtureFails -tags=gatefixture_fail
# or#1013 in miniature: the test exists but its build tag is not requested.
expect_fail  "build tags exclude every test file" "no test files" \
             "${excluded}" TestGateFixtureTagged
expect_pass  "the same package with its tag"      \
             "${excluded}" TestGateFixtureTagged -tags=gatefixture_tag

echo "go-test-gate self-test: all cases passed"
