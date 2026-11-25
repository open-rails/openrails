#!/usr/bin/env bash
# New suite entrypoint. An empty/skipped/partly implemented suite cannot pass.
# A selection (--case OR-JNN-SNN, --journey JNN) runs only those scenarios and
# qualifies nothing: CI's gate takes only a run without one. --shard K/N runs
# shard K of the whole suite; CI proves the N reports together.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off
: "${OPENRAILS_CRITICAL_DSN:?Set OPENRAILS_CRITICAL_DSN to an owned disposable PostgreSQL database}"
: "${OPENRAILS_CRITICAL_CLICKHOUSE_ADDR:?Set OPENRAILS_CRITICAL_CLICKHOUSE_ADDR to an owned disposable ClickHouse (host:port); each scenario creates its own database}"
selected=()
# A shard runs part of the suite: a hung test fails it, with its stack, well inside the job's limit.
timeout=60m
while (($#)); do
  case "$1" in
    --shard) timeout=25m ;&
    --case|--journey) [[ $# -ge 2 ]] || exit 2; selected+=("$1" "$2"); shift 2 ;;
    *) echo 'usage: scripts/critical-tests.sh [--case OR-JNN-SNN ...] [--journey JNN ...] | [--shard K/N]' >&2; exit 2 ;;
  esac
done
report_dir="${OPENRAILS_CRITICAL_REPORT_DIR:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/openrails-critical-$$}"
mkdir -p "$report_dir"
export OPENRAILS_CRITICAL_REPORT_DIR="$report_dir"
run=()
if ((${#selected[@]})); then
  mapfile -t run < <(python3 scripts/critical-results.py critical/coverage.json --test-args "${selected[@]}")
  ((${#run[@]} == 2))
fi
set +e
go test -race -json -tags=integration -count=1 -p 2 -parallel 2 -timeout "$timeout" "${run[@]}" ./critical | tee "$report_dir/events.jsonl"
status=${PIPESTATUS[0]}
set -e
python3 scripts/critical-results.py critical/coverage.json "$report_dir/events.jsonl" "${selected[@]}"
exit "$status"
