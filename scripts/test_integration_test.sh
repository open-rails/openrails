#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/scripts" "$fixture/bin"
cp "$root/scripts/test_integration.sh" "$fixture/scripts/test_integration.sh"
touch "$fixture/docker-compose.yaml"

log="$fixture/integration.log"
export INTEGRATION_TEST_LOG="$log"
printf '%s\n' '#!/bin/sh' '
echo "docker $*" >> "$INTEGRATION_TEST_LOG"
if [ "$1 $2 $3 $4 $5 $6 $7" = "compose -f docker-compose.yaml ps --status running -q" ]; then
    service="$8"
    case " ${FAKE_RUNNING_SERVICES:-} " in *" $service "*) echo "container-$service" ;; esac
fi
' >"$fixture/bin/docker"
printf '%s\n' '#!/bin/sh' 'echo "go $*" >> "$INTEGRATION_TEST_LOG"' 'exit "${FAKE_GO_STATUS:-0}"' >"$fixture/bin/go"
chmod +x "$fixture/bin/docker" "$fixture/bin/go"

run_fixture() {
    : >"$log"
    (
        cd "$fixture"
        PATH="$fixture/bin:$PATH" \
        POSTGRES_HOST_PORT=49161 \
        GARNET_HOST_PORT=49162 \
        bash scripts/test_integration.sh ./fixturepkg
    ) >/dev/null 2>&1
}

unset OPENRAILS_TEST_DB_DSN OPENRAILS_TEST_DB_URL OPENRAILS_TEST_REDIS_ADDR OPENRAILS_KEEP_STACK
unset FAKE_RUNNING_SERVICES FAKE_GO_STATUS
run_fixture
grep -Fq 'docker compose -f docker-compose.yaml up -d --wait postgres garnet' "$log"
grep -Fq 'docker compose -f docker-compose.yaml stop postgres garnet' "$log"

export FAKE_RUNNING_SERVICES='postgres garnet'
run_fixture
grep -Fq 'docker compose -f docker-compose.yaml up -d --wait postgres garnet' "$log"
if grep -Fq 'docker compose -f docker-compose.yaml stop' "$log"; then
    echo "test_integration_test: stopped a pre-existing Compose service" >&2
    exit 1
fi

unset FAKE_RUNNING_SERVICES
export OPENRAILS_TEST_DB_DSN='postgresql://external/db'
export OPENRAILS_TEST_REDIS_ADDR='external:6379'
run_fixture
if grep -Fq 'docker compose' "$log"; then
    echo "test_integration_test: touched Compose with caller-supplied services" >&2
    exit 1
fi

unset OPENRAILS_TEST_REDIS_ADDR
run_fixture
grep -Fq 'docker compose -f docker-compose.yaml up -d --wait garnet' "$log"
grep -Fq 'docker compose -f docker-compose.yaml stop garnet' "$log"
if grep -Eq ' (up|stop) .*postgres' "$log"; then
    echo "test_integration_test: touched Postgres despite a caller-supplied DSN" >&2
    exit 1
fi

unset OPENRAILS_TEST_DB_DSN
export OPENRAILS_KEEP_STACK=1
run_fixture
if grep -Fq 'docker compose -f docker-compose.yaml stop' "$log"; then
    echo "test_integration_test: ignored OPENRAILS_KEEP_STACK=1" >&2
    exit 1
fi

unset OPENRAILS_KEEP_STACK
export FAKE_GO_STATUS=17
set +e
run_fixture
status=$?
set -e
[ "$status" -eq 17 ]
grep -Fq 'docker compose -f docker-compose.yaml stop postgres garnet' "$log"

echo "integration teardown regression tests passed"
