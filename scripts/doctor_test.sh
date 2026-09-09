#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/scripts/hooks" "$fixture/tools"
cp "$root/scripts/doctor.sh" "$fixture/scripts/doctor.sh"
cp "$root/scripts/hooks/pre-commit" "$fixture/scripts/hooks/pre-commit"
cp "$root/go.mod" "$fixture/go.mod"
cp "$root/tools/versions.conf" "$fixture/tools/versions.conf"
cp "$root/docker-compose.yaml" "$fixture/docker-compose.yaml"
git -C "$fixture" init -q

set +e
output="$(
    POSTGRES_HOST_PORT=49151 \
    GARNET_HOST_PORT=49152 \
    OPENRAILS_HOST_PORT=49153 \
    AUTHKIT_ISSUER_HOST_PORT=49154 \
    bash "$fixture/scripts/doctor.sh" 2>&1
)"
status=$?
set -e

[ "$status" -ne 0 ] || {
    echo "doctor_test: an incomplete fixture unexpectedly passed" >&2
    exit 1
}
printf '%s\n' "$output" | grep -q '^FAIL bash '
printf '%s\n' "$output" | grep -q '^FAIL \.env '
printf '%s\n' "$output" | grep -q '^FAIL git hook pre-commit '
printf '%s\n' "$output" | grep -q '; fix: '
printf '%s\n' "$output" | grep -q '^doctor: [0-9][0-9]* check(s) failed$'

cp "$root/.env.example" "$fixture/.env"
chmod +x "$fixture/scripts/hooks/pre-commit"
ln -s "$fixture/scripts/hooks/pre-commit" "$fixture/.git/hooks/pre-commit"
set +e
output="$(
    POSTGRES_HOST_PORT=49151 \
    GARNET_HOST_PORT=49152 \
    OPENRAILS_HOST_PORT=49153 \
    AUTHKIT_ISSUER_HOST_PORT=49154 \
    bash "$fixture/scripts/doctor.sh" 2>&1
)"
set -e
printf '%s\n' "$output" | grep -q '^PASS \.env .*database port and PROVIDER_WRITE_MODE are consistent$'
printf '%s\n' "$output" | grep -q '^PASS git hook pre-commit .*installed from scripts/hooks/pre-commit$'

echo "doctor regression tests passed"
