#!/usr/bin/env bash
# Runs ./ci/... of the root module and of server/: each package's test binary
# is built once, then every top-level test runs in its own process against the
# same disposable database. Every test creates its own schema, so tests never
# share state. A package is named by its directory (ci/subscriptions, server/ci).
#
# OPENRAILS_E2E_WORKERS runners split the tests longest first, each to the
# least-loaded worker, by ci/e2e-durations.tsv (a test missing from it counts
# as the median). Every worker computes the same split, so each test runs on
# exactly one worker. A worker runs OPENRAILS_E2E_JOBS tests at a time, longest
# first. OPENRAILS_E2E_TIMINGS names a file for the measured durations, in the
# durations file's format. To refresh it from CI:
#   gh run download <run> -p 'e2e-timings-*' -D t && sort t/*/*.tsv > ci/e2e-durations.tsv
set -euo pipefail
export LC_ALL=C

cd "$(dirname "$0")/.."
# Each module is proven on its own, never through a developer's go.work.
export GOWORK=off
: "${OPENRAILS_E2E_DSN:?Set OPENRAILS_E2E_DSN to a disposable PostgreSQL database}"
workers="${OPENRAILS_E2E_WORKERS:-1}"
worker="${OPENRAILS_E2E_WORKER:-0}"
jobs="${OPENRAILS_E2E_JOBS:-4}"
if [[ ! "$workers" =~ ^[1-9][0-9]*$ || ! "$jobs" =~ ^[1-9][0-9]*$ || ! "$worker" =~ ^(0|[1-9][0-9]*)$ ]] || ((worker >= workers)); then
  echo "E2E workers and jobs must be positive integers and worker a zero-based index below workers." >&2
  exit 2
fi
durations=ci/e2e-durations.tsv
tags='e2e,integration'
export E2E_TIMEOUT="${OPENRAILS_E2E_TIMEOUT:-8m}"
E2E_OUT="$(mktemp -d)"
export E2E_OUT
trap 'rm -rf "$E2E_OUT"' EXIT
mkdir "$E2E_OUT/logs"

# One build graph per module; a package's binary and directory are filed
# under its name with / as _.
: >"$E2E_OUT/all.tsv"
for module in . server; do
  package_list="$(cd "$module" && go list -tags="$tags" -f '{{.ImportPath}} {{.Dir}}' ./ci/...)"
  mapfile -t packages <<<"$package_list"
  paths=()
  for line in "${packages[@]}"; do
    read -r path _ <<<"$line"
    paths+=("$path")
  done
  mkdir -p "$E2E_OUT/bin/$module"
  (cd "$module" && go test -c -race -vet=all -tags="$tags" -o "$E2E_OUT/bin/$module/" "${paths[@]}")
  for line in "${packages[@]}"; do
    read -r path dir <<<"$line"
    bin="$E2E_OUT/bin/$module/${path##*/}.test"
    [[ -x "$bin" ]] || continue
    name="${dir#"$PWD"/}"
    file="${name//\//_}"
    printf '%s\n' "$dir" >"$E2E_OUT/$file.dir"
    printf '%s\n' "$bin" >"$E2E_OUT/$file.bin"
    listed="$(cd "$dir" && "$bin" -test.list '^Test')"
    { printf '%s\n' "$listed" | grep '^Test' || true; } | awk -v p="$name" '{ print p "\t" $1 }' >>"$E2E_OUT/all.tsv"
  done
done

# Estimate each test, then assign longest first to the least-loaded worker.
median="$({ [[ ! -f "$durations" ]] || cut -f3 "$durations"; } | sort -g | awk '{ v[NR] = $1 } END { print (NR ? v[int((NR + 1) / 2)] : 1) }')"
awk -F'\t' -v OFS='\t' -v d="$median" -v file="$durations" '
    BEGIN { while ((getline line < file) > 0) { split(line, f, "\t"); est[f[1] "\t" f[2]] = f[3] } }
    { k = $1 "\t" $2; print (k in est ? est[k] : d), $1, $2 }' "$E2E_OUT/all.tsv" |
  sort -t$'\t' -k1,1gr -k2,2 -k3,3 |
  awk -F'\t' -v OFS='\t' -v n="$workers" -v me="$worker" -v plan="$E2E_OUT/mine" '
    { b = 0; for (w = 1; w < n; w++) if (load[w] < load[b]) b = w
      load[b] += $1; count[b]++
      if (b == me) print $2, $3 > plan }
    END { close(plan); for (w = 0; w < n; w++) printf "e2e: worker %d/%d: %d tests, ~%ds estimated\n", w, n, count[w], load[w] }'
touch "$E2E_OUT/mine"
echo "e2e: $(wc -l <"$E2E_OUT/all.tsv") tests in all; worker $worker runs $(wc -l <"$E2E_OUT/mine"), $jobs at a time"

run_one() {
  local name=$1 test=$2 status=0 start secs log file
  file="${name//\//_}"
  log="$E2E_OUT/logs/$file.$test.log"
  start=$EPOCHREALTIME
  (cd "$(<"$E2E_OUT/$file.dir")" && "$(<"$E2E_OUT/$file.bin")" -test.count=1 -test.parallel=4 \
    -test.timeout="$E2E_TIMEOUT" -test.run="^$test\$") >"$log" 2>&1 || status=$?
  if ((status == 0)) && grep -q 'no tests to run' "$log"; then
    echo "e2e: -run ^$test\$ matched no test" >>"$log"
    status=1
  fi
  secs="$(awk -v a="$start" -v b="$EPOCHREALTIME" 'BEGIN { printf "%.1f", b - a }')"
  printf '%s\t%s\t%s\t%s\n' "$name" "$test" "$secs" "$status" >>"$E2E_OUT/results.tsv"
  if ((status == 0)); then echo "e2e: ok   $name $test ${secs}s"; else echo "e2e: FAIL $name $test ${secs}s"; fi
}
export -f run_one
: >"$E2E_OUT/results.tsv"
xargs -r -P "$jobs" -n 2 bash -c 'run_one "$1" "$2"' _ <"$E2E_OUT/mine" || true

status=0
while IFS=$'\t' read -r name test secs result; do
  [[ "$result" == 0 ]] || continue
  echo "::group::ok $name $test (${secs}s)"
  cat "$E2E_OUT/logs/${name//\//_}.$test.log"
  echo "::endgroup::"
done <"$E2E_OUT/results.tsv"
while IFS=$'\t' read -r name test secs result; do
  [[ "$result" != 0 ]] || continue
  echo "FAIL $name $test (${secs}s)"
  cat "$E2E_OUT/logs/${name//\//_}.$test.log"
  status=1
done <"$E2E_OUT/results.tsv"
planned="$(wc -l <"$E2E_OUT/mine")"
ran="$(wc -l <"$E2E_OUT/results.tsv")"
if ((ran != planned)); then
  echo "e2e: ran $ran of $planned planned tests" >&2
  status=1
fi
echo "e2e: slowest on worker $worker:"
sort -t$'\t' -k3,3gr "$E2E_OUT/results.tsv" | awk -F'\t' 'NR <= 15 { printf "  %6.1fs  %s %s\n", $3, $1, $2 }'
echo "e2e: load average $(cut -d' ' -f1-3 /proc/loadavg 2>/dev/null || echo '?') on $(nproc 2>/dev/null || echo '?') CPUs"
if [[ -n "${OPENRAILS_E2E_TIMINGS:-}" ]]; then
  cut -f1-3 "$E2E_OUT/results.tsv" | sort >"$OPENRAILS_E2E_TIMINGS"
fi
exit "$status"
