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
export INTEGRATION_REAL_GO="$(command -v go)"
printf '%s\n' '#!/bin/sh' '
echo "docker $*" >> "$INTEGRATION_TEST_LOG"
if [ "$1 $2 $3 $4 $5 $6 $7" = "compose -f docker-compose.yaml ps --status running -q" ]; then
    service="$8"
    case " ${FAKE_RUNNING_SERVICES:-} " in *" $service "*) echo "container-$service" ;; esac
fi
' >"$fixture/bin/docker"
cat >"$fixture/bin/go" <<'GO'
#!/bin/sh
echo "go $*" >> "$INTEGRATION_TEST_LOG"
if [ "$1" = "test" ]; then
    echo "redis ${OPENRAILS_TEST_REDIS_ADDR-unset}" >> "$INTEGRATION_TEST_LOG"
fi
if [ "$1" = "list" ]; then
    [ "${FAKE_GO_LIST_FAIL:-0}" != 1 ] || exit 17
    [ "${FAKE_GO_LIST_EMPTY:-0}" != 1 ] || exit 0
    exec "$INTEGRATION_REAL_GO" "$@"
fi
exit "${FAKE_GO_STATUS:-0}"
GO
chmod +x "$fixture/bin/docker" "$fixture/bin/go"

run_fixture() {
    : >"$log"
    (
        cd "$fixture"
        PATH="$fixture/bin:$PATH" \
        POSTGRES_HOST_PORT=49161 \
        GARNET_HOST_PORT=49162 \
        bash scripts/test_integration.sh "$@" ./fixturepkg
    ) >/dev/null 2>&1
}

unset OPENRAILS_TEST_DB_DSN OPENRAILS_TEST_DB_URL OPENRAILS_TEST_REDIS_ADDR OPENRAILS_KEEP_STACK OPENRAILS_TEST_PACKAGES
unset FAKE_RUNNING_SERVICES FAKE_GO_STATUS
run_fixture
grep -Fq 'docker compose -f docker-compose.yaml up -d --wait postgres garnet' "$log"
grep -Fq 'docker compose -f docker-compose.yaml stop postgres garnet' "$log"
grep -Fq 'go test -vet=all -race -count=1 -p 1 -parallel 1 -tags=integration' "$log"

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

# Independent package processes may run concurrently only with private Redis.
unset FAKE_GO_STATUS
export OPENRAILS_TEST_PACKAGES=3
export OPENRAILS_TEST_DB_DSN='postgresql://external/db'
unset OPENRAILS_TEST_REDIS_ADDR
run_fixture
grep -Fq 'go test -vet=all -race -count=1 -p 3 -parallel 1' "$log"
grep -Fxq 'redis unset' "$log"
if grep -Fq 'docker compose' "$log"; then
    echo "test_integration_test: parallel mode started unused shared services" >&2; exit 1
fi
unset OPENRAILS_TEST_DB_DSN
run_fixture
grep -Fqx 'docker compose -f docker-compose.yaml up -d --wait postgres' "$log"
grep -Fqx 'docker compose -f docker-compose.yaml stop postgres' "$log"

export OPENRAILS_TEST_REDIS_ADDR='external:6379'
if run_fixture; then
    echo "test_integration_test: parallel packages accepted shared Redis" >&2; exit 1
fi
[[ ! -s "$log" ]] || { echo "unsafe parallel mode touched services" >&2; exit 1; }
unset OPENRAILS_TEST_REDIS_ADDR
export OPENRAILS_TEST_PACKAGES=zero
if run_fixture; then
    echo "test_integration_test: invalid package count accepted" >&2; exit 1
fi
[[ ! -s "$log" ]] || { echo "invalid count touched services" >&2; exit 1; }
unset OPENRAILS_TEST_PACKAGES
for override in -p=3 --p=3 -parallel=2 --parallel=2 -test.parallel=2 --test.parallel=2; do
    if run_fixture "$override"; then
        echo "test_integration_test: parallelism override accepted: $override" >&2; exit 1
    fi
    [[ ! -s "$log" ]] || { echo "parallelism override touched services" >&2; exit 1; }
done

echo "integration teardown regression tests passed"

# Inspect real Go build metadata without running tests or starting services.
# Default-only test AND production variants must keep their entire package in
# Checks; an ordinary mixed package must move to E2E with its unit tests intact.
unset FAKE_GO_STATUS FAKE_GO_LIST_FAIL FAKE_GO_LIST_EMPTY
printf 'module example.test/partition\n\ngo 1.26.0\n' >"$fixture/go.mod"
for package in ordinary mixed defaulttest defaultprod; do
    mkdir -p "$fixture/$package"
    printf 'package %s\n' "$package" >"$fixture/$package/source.go"
    printf 'package %s\nimport "testing"\nfunc TestUnit(t *testing.T) {}\n' "$package" >"$fixture/$package/unit_test.go"
done
for package in mixed defaulttest defaultprod integrationonly; do
    mkdir -p "$fixture/$package"
    printf '//go:build integration\n\npackage %s\n' "$package" >"$fixture/$package/integration.go"
done
printf '//go:build !integration\n\npackage defaulttest\nimport "testing"\nfunc TestDefaultOnly(t *testing.T) {}\n' >"$fixture/defaulttest/default_test.go"
printf '//go:build !integration\n\npackage defaultprod\nconst DefaultOnly = true\n' >"$fixture/defaultprod/default.go"

# Native adapters are separate modules. Their integration files must not enter
# the root-module package partition, even when no root integration files remain.
mkdir -p "$fixture/adapters/nested"
printf 'module example.test/partition/adapters/nested\n\ngo 1.26.0\n' >"$fixture/adapters/nested/go.mod"
printf '//go:build integration\n\npackage nested\n' >"$fixture/adapters/nested/integration.go"

run_selection() {
    : >"$log"
    (cd "$fixture"; PATH="$fixture/bin:$PATH" GOWORK=off bash scripts/test_integration.sh "$1")
}

selected="$(run_selection --list-checks-packages)"
expected="$(printf '%s\n' example.test/partition/defaultprod example.test/partition/defaulttest example.test/partition/ordinary)"
[[ "$selected" == "$expected" ]] || { echo "wrong Checks partition: $selected" >&2; exit 1; }
if grep -Eq '^docker |^go test ' "$log"; then
    echo "test_integration_test: selecting packages started services or tests" >&2
    exit 1
fi
grep -Fq 'go list -race -tags=integration,browser' "$log"

selected="$(run_selection --list-integration-packages)"
expected="$(printf '%s\n' ./defaultprod ./defaulttest ./integrationonly ./mixed)"
[[ "$selected" == "$expected" ]] || { echo "wrong E2E partition: $selected" >&2; exit 1; }
[[ ! -s "$log" ]] || { echo "integration selection unexpectedly invoked Go or Docker" >&2; exit 1; }

export FAKE_GO_LIST_FAIL=1
if run_selection --list-checks-packages >"$fixture/failure" 2>&1; then
    echo "test_integration_test: failed metadata was accepted" >&2; exit 1
fi
unset FAKE_GO_LIST_FAIL
export FAKE_GO_LIST_EMPTY=1
if run_selection --list-checks-packages >"$fixture/failure" 2>&1; then
    echo "test_integration_test: empty metadata was accepted" >&2; exit 1
fi
grep -Fq 'empty Go package metadata' "$fixture/failure"
unset FAKE_GO_LIST_EMPTY

# Scheduling changes order only: every selected package occurs exactly once.
export OPENRAILS_TEST_PACKAGES=4 OPENRAILS_TEST_DB_DSN='postgresql://external/db'
for package in embed internal/app internal/river internal/integrationharness; do
    mkdir -p "$fixture/$package"
    printf '//go:build integration\n\npackage fixture\n' >"$fixture/$package/integration.go"
done
run_fixture ./...
command="$(grep '^go test ' "$log")"
[[ "$command" == *" ./internal/integrationharness ./internal/app ./internal/river ./embed ./defaultprod ./defaulttest ./integrationonly ./mixed ./fixturepkg" ]] || {
    echo "test_integration_test: scheduling lost or duplicated a package: $command" >&2; exit 1
}
unset OPENRAILS_TEST_PACKAGES OPENRAILS_TEST_DB_DSN
rm -rf "$fixture/embed" "$fixture/internal"

rm -rf "$fixture/mixed" "$fixture/defaulttest" "$fixture/defaultprod" "$fixture/integrationonly"
if run_selection --list-checks-packages >"$fixture/failure" 2>&1; then
    echo "test_integration_test: absent integration packages were accepted" >&2; exit 1
fi
grep -Fq 'no packages carry' "$fixture/failure"
echo "integration package partition regression tests passed"
