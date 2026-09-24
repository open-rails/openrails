#!/usr/bin/env bash
# Runs ./ci/greenfield/... : each package's test binary is built once, then its
# top-level tests run in OPENRAILS_GREENFIELD_SHARDS concurrent processes
# (round-robin by name) against the same disposable database. Every test
# creates its own schema, so shards never share state.
set -euo pipefail

cd "$(dirname "$0")/.."
: "${OPENRAILS_GREENFIELD_DSN:?Set OPENRAILS_GREENFIELD_DSN to a disposable PostgreSQL database}"
shards="${OPENRAILS_GREENFIELD_SHARDS:-1}"
timeout="${OPENRAILS_GREENFIELD_TIMEOUT:-8m}"
tags='greenfield,integration'
out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT

mapfile -t packages < <(go list -tags="$tags" ./ci/greenfield/...)
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
  mapfile -t tests < <(cd "$dir" && "$bin" -test.list '^Test' | grep '^Test' || true)
  [ "${#tests[@]}" -gt 0 ] || continue
  n="$shards"
  [ "${#tests[@]}" -lt "$n" ] && n="${#tests[@]}"
  for ((s = 0; s < n; s++)); do
    run=()
    for ((i = s; i < ${#tests[@]}; i += n)); do run+=("${tests[i]}"); done
    pattern="^($(IFS='|'; echo "${run[*]}"))\$"
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
