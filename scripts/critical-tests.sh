#!/usr/bin/env bash
# New suite entrypoint. An empty/skipped/partly implemented suite cannot pass.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off
: "${OPENRAILS_CRITICAL_DSN:?Set OPENRAILS_CRITICAL_DSN to an owned disposable PostgreSQL database}"
selected=()
suite=all
while (($#)); do
  case "$1" in
    --case) [[ $# -ge 2 ]] || exit 2; selected+=("$2"); shift 2 ;;
    --suite) [[ $# -ge 2 ]] || exit 2; suite="$2"; shift 2 ;;
    *) echo 'usage: scripts/critical-tests.sh [--suite all|browser | --case OR-JNN-SNN ...]' >&2; exit 2 ;;
  esac
done
if [[ ${CI:-} == true && ${#selected[@]} -gt 0 ]]; then
  echo 'Focused local cases cannot satisfy the hosted CI gate.' >&2
  exit 2
fi
case "$suite" in
  all) ;;
  browser)
    ((${#selected[@]} == 0)) || { echo 'A suite cannot be combined with ad hoc cases.' >&2; exit 2; }
    browser_cases="$(python3 -c 'import json; print("\n".join(json.load(open("critical/executables.json"))["suites"]["browser"]))')"
    [[ -n "$browser_cases" ]] || { echo 'Reviewed browser suite is empty.' >&2; exit 2; }
    mapfile -t selected <<<"$browser_cases"
    ;;
  *) echo 'Unknown critical suite.' >&2; exit 2 ;;
esac
report_dir="${OPENRAILS_CRITICAL_REPORT_DIR:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/openrails-critical-$$}"
mkdir -p "$report_dir"
export OPENRAILS_CRITICAL_REPORT_DIR="$report_dir"
run=()
check=()
if ((${#selected[@]})); then
  pattern=""
  for identifier in "${selected[@]}"; do
    [[ "$identifier" =~ ^OR-J[0-9]{2}-S[0-9]{2}$ ]] || exit 2
    pattern+="${pattern:+|}$identifier"
    if [[ "$suite" == all ]]; then check+=(--case "$identifier"); fi
  done
  run=(-run "^Test/($pattern)$")
fi
if [[ "$suite" == browser ]]; then check=(--suite browser); fi
set +e
go test -race -json -tags=integration -count=1 -p 2 -parallel 2 -timeout 60m "${run[@]}" ./critical | tee "$report_dir/events.jsonl"
status=${PIPESTATUS[0]}
set -e
python3 scripts/critical-results.py critical/coverage.json "$report_dir/events.jsonl" "${check[@]}"
exit "$status"
