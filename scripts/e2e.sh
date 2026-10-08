#!/usr/bin/env bash
# Runs ./ci/... : each package's test binary is built once, then its
# top-level tests run in OPENRAILS_E2E_SHARDS concurrent processes
# (round-robin by name) against the same disposable database. Every test
# creates its own schema, so shards never share state. Optional workers stripe
# those same global shard IDs across separate CI runners; no test is duplicated.
set -euo pipefail

cd "$(dirname "$0")/.."
: "${OPENRAILS_E2E_DSN:?Set OPENRAILS_E2E_DSN to a disposable PostgreSQL database}"
shards="${OPENRAILS_E2E_SHARDS:-1}"
workers="${OPENRAILS_E2E_WORKERS:-1}"
worker="${OPENRAILS_E2E_WORKER:-0}"
if [[ ! "$shards" =~ ^[1-9][0-9]*$ || ! "$workers" =~ ^[1-9][0-9]*$ || ! "$worker" =~ ^(0|[1-9][0-9]*)$ ]]; then
  echo "E2E shards/workers must be positive integers; worker must be a zero-based integer." >&2
  exit 2
fi
if ((workers > shards || worker >= workers)); then
  echo "E2E requires worker < workers <= shards." >&2
  exit 2
fi
timeout="${OPENRAILS_E2E_TIMEOUT:-8m}"
tags='e2e,integration'
out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT

package_list="$(go list -tags="$tags" ./ci/...)"
mapfile -t packages <<< "$package_list"
for pkg in "${packages[@]}"; do
  go test -c -race -vet=all -tags="$tags" -o "$out/$(basename "$pkg").test" "$pkg"
done

pids=()
names=()
logs=()
for pkg in "${packages[@]}"; do
  name="$(basename "$pkg")"
  bin="$out/$name.test"
  dir="$(go list -tags="$tags" -f '{{.Dir}}' "$pkg")"
  listed="$(cd "$dir" && "$bin" -test.list '^Test')"
  mapfile -t tests < <(printf '%s\n' "$listed" | grep '^Test' || true)
  [ "${#tests[@]}" -gt 0 ] || continue
  n="$shards"
  [ "${#tests[@]}" -lt "$n" ] && n="${#tests[@]}"
  for ((s = 0; s < n; s++)); do
    ((s % workers == worker)) || continue
    run=()
    for ((i = s; i < ${#tests[@]}; i += n)); do run+=("${tests[i]}"); done
    pattern="^($(IFS='|'; echo "${run[*]}"))\$"
    echo "e2e: worker $worker/$workers runs $name shard $s/$n (${#run[@]} top-level tests)"
    log="$out/$name.$s.log"
    logs+=("$log")
    (cd "$dir" && "$bin" -test.count=1 -test.parallel=4 -test.timeout="$timeout" -test.run="$pattern" >"$log" 2>&1) &
    pids+=("$!")
    names+=("$name shard $s/$n")
  done
done

status=0
for i in "${!pids[@]}"; do
  if wait "${pids[i]}"; then
    echo "::group::ok ${names[i]}"
    cat "${logs[i]}"
    echo "::endgroup::"
  else
    echo "FAIL ${names[i]}"
    cat "${logs[i]}"
    status=1
  fi
done
exit "$status"
