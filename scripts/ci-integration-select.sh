#!/usr/bin/env bash
# or#855: pick the integration-tagged packages THIS CI job runs.
#
#   scripts/ci-integration-select.sh
#
# Reads the environment (all optional):
#   BROAD=true       run the full tagged set, ignoring CHANGED_FILES.
#   CHANGED_FILES    newline-separated diff paths; the set is narrowed to the
#                    tagged packages those paths live in. Unset => full set.
#   SHARDS=n SHARD=i emit only slice i (1-based) of n.
#
# Prints ./-prefixed package paths, one per line, for scripts/test_integration.sh.
# Prints nothing (exit 0) when the diff touches no tagged package.
#
# SHARDING IS COVERAGE-NEUTRAL. The tests are unchanged and the union of the
# shards is exactly the set a single job would have run. Each shard is a
# separate runner with its own compose stack, so the `-p 1 -parallel 1`
# serialisation the suite depends on — all packages share one Postgres/Garnet
# and Redis keys are NOT namespaced per package — is preserved WITHIN a shard
# and never crosses one. What sharding buys is wall clock: the serial run was
# ~360s and the pipeline's sole long pole.
#
# Balancing uses recorded runtimes (scripts/ci-integration-weights.txt) and
# longest-processing-time-first bin packing, plus a flat per-package constant
# standing for compiling and linking that package's test binary. A package with
# no recorded time still runs; it just gets the default weight.
set -euo pipefail
cd "$(dirname "$0")/.."

weights_file="scripts/ci-integration-weights.txt"
# Compile+link of one test binary, in weight units (seconds). The serial job
# spent ~360s to run ~210s of tests, so per-package build cost is real and a
# shard of many fast packages is not free.
per_package_build_cost=3
default_runtime=5

tag_re='^//go:build (.*[^a-zA-Z0-9_])?integration([^a-zA-Z0-9_].*)?$'

packages="$(
  grep -rl --include='*.go' -E "$tag_re" . |
    xargs -n1 dirname | sed 's|^\./||' | sort -u
)"

[ -n "$packages" ] || {
  echo "ci-integration-select: no package carries the 'integration' build tag; refusing to test nothing" >&2
  exit 1
}

# Narrow to the packages the diff touches, unless the diff hit a shared surface.
if [ "${BROAD:-}" != "true" ] && [ -n "${CHANGED_FILES:-}" ]; then
  packages="$(
    CHANGED="$CHANGED_FILES" awk '
      BEGIN {
        n = split(ENVIRON["CHANGED"], lines, "\n")
        for (i = 1; i <= n; i++) {
          f = lines[i]
          if (f == "") continue
          sub(/\/[^\/]*$/, "", f)
          touched[f] = 1
        }
      }
      { for (d in touched) if (d == $1 || index(d "/", $1 "/") == 1) { print; next } }
    ' <<<"$packages"
  )"
  if [ -z "$packages" ]; then
    echo "ci-integration-select: no integration-tagged package touched by the diff" >&2
    exit 0
  fi
fi

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
  { pkg[NR] = $1; w[NR] = build_cost + (($1 in recorded) ? recorded[$1] : default_runtime) }
  END {
    # Heaviest first (insertion sort over a list this small), then each package
    # onto the lightest shard so far.
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
      if (best == pick) print "./" pkg[p]
    }
  }
' <<<"$packages" | sort
