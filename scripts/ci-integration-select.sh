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
# Balancing is delegated to scripts/ci-shard.sh (recorded runtimes + LPT bin
# packing), which the unit suite uses too — one implementation, two weight
# tables.
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

# Packages the diff can NEVER exempt (or#931). A fence whose input is the WHOLE
# assembled surface — internal/integrationharness pins the standalone route
# table against a golden — is affected by a change to any route registration in
# the tree, and by construction its own directory is not in that diff. Narrowing
# it by diff means it only ever runs post-merge, which is how or#930 added
# `GET /v1/me/spend-limits` with six green shards and reddened master.
always_run="internal/integrationharness"

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
  for pkg in $always_run; do
    if ! printf '%s\n' "$packages" | grep -qxF "$pkg"; then
      packages="$(printf '%s\n%s\n' "$packages" "$pkg" | grep -v '^$' | sort -u)"
    fi
  done
fi

printf '%s\n' "$packages" | SHARDS="${SHARDS:-1}" SHARD="${SHARD:-1}" \
  bash scripts/ci-shard.sh scripts/ci-integration-weights.txt |
  sed 's|^|./|' | sort
