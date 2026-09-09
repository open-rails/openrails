#!/usr/bin/env bash
set -euo pipefail

# Packages containing files (test or non-test) that carry the `integration`
# build tag. Running `./...` verbatim would serially re-run every untagged
# unit test the build job already ran in parallel, so `./...` (and no args)
# expands to only the tagged packages. Explicit flags/packages pass through.
integration_packages() {
  grep -rl --include='*.go' -E '^//go:build (.*[^a-zA-Z0-9_])?integration([^a-zA-Z0-9_].*)?$' . |
    xargs -n1 dirname | sed -e 's|^\./||' -e 's|^|./|' | sort -u
}

if [ "$#" -eq 0 ]; then
  set -- ./...
fi

args=()
for arg in "$@"; do
  if [ "$arg" = "./..." ]; then
    pkgs=()
    while IFS= read -r pkg; do
      [ -n "$pkg" ] && pkgs+=("$pkg")
    done < <(integration_packages)
    if [ "${#pkgs[@]}" -eq 0 ]; then
      echo "test_integration.sh: no packages carry the 'integration' build tag; refusing to test nothing" >&2
      exit 1
    fi
    echo "test_integration.sh: ./... -> ${#pkgs[@]} integration-tagged packages" >&2
    args+=("${pkgs[@]}")
  else
    args+=("$arg")
  fi
done

export POSTGRES_HOST_PORT="${POSTGRES_HOST_PORT:-5434}"
export GARNET_HOST_PORT="${GARNET_HOST_PORT:-6380}"

port_is_listening() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1
}

compose_postgres_owns_port() {
  docker compose -f docker-compose.yaml port postgres 5432 2>/dev/null |
    grep -Eq ":$1$"
}

compose_service_running() {
  [[ -n "$(docker compose -f docker-compose.yaml ps --status running -q "$1" 2>/dev/null)" ]]
}

compose_services=()
started_services=()

if [[ -z "${OPENRAILS_TEST_DB_DSN:-${OPENRAILS_TEST_DB_URL:-}}" ]]; then
  if port_is_listening "${POSTGRES_HOST_PORT}" && ! compose_postgres_owns_port "${POSTGRES_HOST_PORT}"; then
    echo "test_integration.sh: host port ${POSTGRES_HOST_PORT} is already held by a non-Compose process." >&2
    echo "Set POSTGRES_HOST_PORT to a free port (and let .env derive DB_URL/DB_PORT from it), or stop the listener." >&2
    exit 1
  fi
  compose_services+=(postgres)
  compose_service_running postgres || started_services+=(postgres)
fi

if [[ -z "${OPENRAILS_TEST_REDIS_ADDR:-}" ]]; then
  compose_services+=(garnet)
  compose_service_running garnet || started_services+=(garnet)
fi

cleanup_started_services() {
  status=$?
  trap - EXIT
  if ! docker compose -f docker-compose.yaml stop "${started_services[@]}"; then
    echo "test_integration.sh: failed to stop started Compose dependencies: ${started_services[*]}" >&2
    [[ "$status" -ne 0 ]] || status=1
  fi
  exit "$status"
}

if [[ "${#compose_services[@]}" -gt 0 ]]; then
  if [[ "${OPENRAILS_KEEP_STACK:-0}" != "1" && "${#started_services[@]}" -gt 0 ]]; then
    trap cleanup_started_services EXIT
  fi
  docker compose -f docker-compose.yaml up -d --wait "${compose_services[@]}"
fi

export OPENRAILS_TEST_DB_DSN="${OPENRAILS_TEST_DB_DSN:-${OPENRAILS_TEST_DB_URL:-postgresql://admin:admin_password@127.0.0.1:${POSTGRES_HOST_PORT}/openrails_db?sslmode=disable}}"
export OPENRAILS_TEST_REDIS_ADDR="${OPENRAILS_TEST_REDIS_ADDR:-127.0.0.1:${GARNET_HOST_PORT}}"

# -count=1 is MANDATORY, not stylistic. Go's test-result cache keys on package
# content, env vars and files read — it cannot see the Postgres/Garnet stack
# these tests actually exercise, so a cached `ok` is a pass that was never
# re-earned against the current schema and data. Observed live (or#855): with a
# warm GOCACHE, whole integration packages came back `ok … (cached)` without a
# single query running.
go test -count=1 -p 1 -parallel 1 -tags=integration -timeout "${OPENRAILS_INTEGRATION_TIMEOUT:-25m}" "${args[@]}"
