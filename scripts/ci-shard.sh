#!/usr/bin/env bash
# or#855: split a package list across CI shards, balanced by recorded runtimes.
#
#   <package list on stdin> | scripts/ci-shard.sh <weights-file>
#
# Environment:
#   SHARDS=n SHARD=i   emit only slice i (1-based) of n. Defaults 1/1.
#
# Input is one package per line, in any form; the FIRST field is matched against
# the weights file, whose lines are `<package><TAB><seconds>` with `#` comments.
#
# Longest-processing-time-first bin packing: heaviest package onto the lightest
# shard so far. Round-robin over an alphabetical list was tried and is much
# worse — it put the two heaviest packages in the same shard and made it 2x the
# others (measured: unit shard 3 at 136s against 64s and 65s).
#
# A package with no recorded weight still runs; it just gets the default. So a
# stale weights file costs balance, never coverage.
set -euo pipefail
cd "$(dirname "$0")/.."

weights_file="${1:?usage: ci-shard.sh <weights-file>}"

# Compile+link of one test binary, in weight units (seconds). Without it a shard
# of many trivial packages looks free and gets overloaded.
per_package_build_cost=3
default_runtime=5

awk -v shards="${SHARDS:-1}" -v pick="${SHARD:-1}" \
    -v build_cost="$per_package_build_cost" -v default_runtime="$default_runtime" \
    -v weights_file="$weights_file" '
  BEGIN {
    while ((getline line < weights_file) > 0) {
      if (line ~ /^#/ || line == "") continue
      split(line, kv, "\t")
      recorded[kv[1]] = kv[2] + 0
    }
  }
  {
    key = $1
    sub(/^\.\//, "", key)
    sub(/^github\.com\/open-rails\/openrails\/?/, "", key)
    if (key == "") key = "."
    pkg[NR] = $1
    w[NR] = build_cost + ((key in recorded) ? recorded[key] : default_runtime)
  }
  END {
    for (i = 1; i <= NR; i++) order[i] = i
    for (i = 2; i <= NR; i++) {
      k = order[i]
      for (j = i - 1; j >= 1 && w[order[j]] < w[k]; j--) order[j + 1] = order[j]
      order[j + 1] = k
    }
    for (i = 1; i <= NR; i++) {
      p = order[i]; best = 1
      for (s = 2; s <= shards; s++) if (load[s] < load[best]) best = s
      load[best] += w[p]
      if (best == pick) print pkg[p]
    }
  }
'
