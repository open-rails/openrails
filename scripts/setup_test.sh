#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/scripts/hooks" "$fixture/bin"
cp "$root/scripts/setup.sh" "$fixture/scripts/setup.sh"
cp "$root/scripts/install-git-hooks.sh" "$fixture/scripts/install-git-hooks.sh"
cp "$root/scripts/hooks/pre-commit" "$fixture/scripts/hooks/pre-commit"
cp "$root/.env.example" "$fixture/.env.example"
git -C "$fixture" init -q

log="$fixture/setup.log"
export SETUP_TEST_LOG="$log"

printf '%s\n' '#!/bin/sh' \
    '[ -f .env ] || { echo "doctor ran before .env existed" >&2; exit 1; }' \
    '[ -L .git/hooks/pre-commit ] || { echo "doctor ran before hooks were installed" >&2; exit 1; }' \
    'echo doctor >> "$SETUP_TEST_LOG"' >"$fixture/scripts/doctor.sh"
printf '%s\n' '#!/bin/sh' 'echo "go $*" >> "$SETUP_TEST_LOG"' >"$fixture/bin/go"
printf '%s\n' '#!/bin/sh' 'echo "docker $*" >> "$SETUP_TEST_LOG"' >"$fixture/bin/docker"
chmod +x "$fixture/scripts/doctor.sh" "$fixture/bin/go" "$fixture/bin/docker"

PATH="$fixture/bin:$PATH" bash "$fixture/scripts/setup.sh" >/dev/null
cmp -s "$fixture/.env.example" "$fixture/.env"
[ "$(sed -n '1p' "$log")" = "doctor" ]
[ "$(sed -n '2p' "$log")" = "go mod download" ]
[ "$(sed -n '3p' "$log")" = "docker volume create go_mod_cache_persistent" ]

printf 'KEEP_ME=true\n' >"$fixture/.env"
: >"$log"
PATH="$fixture/bin:$PATH" bash "$fixture/scripts/setup.sh" >/dev/null
[ "$(cat "$fixture/.env")" = "KEEP_ME=true" ]

echo "setup regression tests passed"
