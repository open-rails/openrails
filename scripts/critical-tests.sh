#!/usr/bin/env bash
# New suite entrypoint. An empty/skipped/partly implemented suite cannot pass.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off
: "${OPENRAILS_CRITICAL_DSN:?Set OPENRAILS_CRITICAL_DSN to an owned disposable PostgreSQL database}"
selected=()
while (($#)); do
  case "$1" in
    --case) [[ $# -ge 2 ]] || exit 2; selected+=("$2"); shift 2 ;;
    *) echo 'usage: scripts/critical-tests.sh [--case OR-JNN-SNN ...]' >&2; exit 2 ;;
  esac
done
if [[ ${CI:-} == true && ${#selected[@]} -gt 0 ]]; then
  echo 'Focused local cases cannot satisfy the hosted CI gate.' >&2
  exit 2
fi
report_dir="${OPENRAILS_CRITICAL_REPORT_DIR:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/openrails-critical-$$}"
mkdir -p "$report_dir"
run=()
check=()
if ((${#selected[@]})); then
  pattern=""
  for identifier in "${selected[@]}"; do
    [[ "$identifier" =~ ^OR-J[0-9]{2}-S[0-9]{2}$ ]] || exit 2
    pattern+="${pattern:+|}$identifier"
    check+=(--case "$identifier")
  done
  run=(-run "^Test/($pattern)$")
fi
set +e
go test -json -tags=integration -count=1 -p 2 -parallel 2 -timeout 20m "${run[@]}" ./critical | tee "$report_dir/events.jsonl"
status=${PIPESTATUS[0]}
set -e
python3 scripts/critical-results.py critical/coverage.json "$report_dir/events.jsonl" "${check[@]}"
exit "$status"
