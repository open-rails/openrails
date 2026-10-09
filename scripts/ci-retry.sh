#!/usr/bin/env bash
# Runs a command up to 5 times, sleeping 2/4/8/16s plus 0-3s of jitter between
# tries, for registry and auth calls that fail transiently in CI (rate limits,
# token-service timeouts). The command must be safe to repeat.
#
#   scripts/ci-retry.sh <command> [args...]
set -uo pipefail

[ "$#" -gt 0 ] || { echo "usage: ci-retry.sh <command> [args...]" >&2; exit 2; }

attempts=5
for ((try = 1; ; try++)); do
    "$@" && exit 0
    status=$?
    if ((try == attempts)); then
        echo "ci-retry: '$1' failed $attempts times" >&2
        exit "$status"
    fi
    delay=$((2 ** try + RANDOM % 4))
    echo "ci-retry: '$1' exited $status (try $try/$attempts); retrying in ${delay}s" >&2
    sleep "$delay"
done
