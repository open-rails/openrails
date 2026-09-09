#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/scripts/hooks" "$fixture/bin"
cp "$root/scripts/hooks/pre-push" "$fixture/scripts/hooks/pre-push"
git -C "$fixture" init -q

log="$fixture/pre-push.log"
export PRE_PUSH_TEST_LOG="$log"
printf '%s\n' '#!/bin/sh' 'echo "go $*" >> "$PRE_PUSH_TEST_LOG"' >"$fixture/bin/go"
printf '%s\n' '#!/bin/sh' 'echo "task $*" >> "$PRE_PUSH_TEST_LOG"' >"$fixture/bin/task"
printf '%s\n' '#!/bin/sh' 'echo business-time >> "$PRE_PUSH_TEST_LOG"' >"$fixture/scripts/check_business_time.sh"
chmod +x "$fixture/bin/go" "$fixture/bin/task" "$fixture/scripts/check_business_time.sh"

(
    cd "$fixture"
    PATH="$fixture/bin:$PATH" sh scripts/hooks/pre-push
)

[ "$(sed -n '1p' "$log")" = "go build ./..." ]
[ "$(sed -n '2p' "$log")" = "go vet -tags integration ./..." ]
[ "$(sed -n '3p' "$log")" = "business-time" ]
[ "$(sed -n '4p' "$log")" = "task sqlc-generate-check" ]
[ "$(wc -l <"$log" | tr -d '[:space:]')" = "4" ]

echo "pre-push regression tests passed"
