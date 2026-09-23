#!/usr/bin/env bash
set -euo pipefail

# Packages containing files (test or non-test) that carry the `integration`
# build tag. These packages run their unit and integration tests together;
# Checks selects the remaining packages, preserving default-only variants.
# `./...` (and no args) expands to this set; explicit arguments pass through.
integration_packages() {
  # Match Go's ./... module boundary. Nested adapters run through their own
  # workspace check; passing their paths to the root module's go list fails.
  find . -mindepth 1 \
    \( -type d \( -name .git -o -name node_modules -o -name vendor -o -exec test -f '{}/go.mod' \; \) -prune \) -o \
    \( -type f -name '*.go' -print0 \) |
    xargs -0 -r grep -l -E '^//go:build (.*[^a-zA-Z0-9_])?integration([^a-zA-Z0-9_].*)?$' |
    xargs -n1 dirname | sed -e 's|^\./||' -e 's|^|./|' | sort -u
}

selected_integration_packages() {
  local packages
  if ! packages="$(integration_packages)" || [[ -z "$packages" ]]; then
    echo "test_integration.sh: no packages carry the 'integration' build tag; refusing to test nothing" >&2
    return 1
  fi
  printf '%s\n' "$packages"
}

# Use Go's selected file lists, not test-name patterns. A package stays in
# Checks when the E2E build omits any default source or test file, including
# future !integration variants. Such packages intentionally run in both builds.
checks_packages() {
  local packages package template field default_files e2e_files selected
  local -a targets=()
  packages="$(selected_integration_packages)"
  while IFS= read -r package; do
    [[ -n "$package" ]] && targets+=("$package")
  done <<< "$packages"
  template='{{.ImportPath}}'
  for field in GoFiles CgoFiles CFiles CXXFiles MFiles HFiles FFiles SFiles SwigFiles SwigCXXFiles SysoFiles TestGoFiles XTestGoFiles; do
    template+="{{range .$field}} {{.}}{{end}}"
  done
  default_files="$(go list -race -f "$template" ./...)"
  e2e_files="$(go list -race -tags=integration,browser -f "$template" "${targets[@]}")"
  if [[ -z "$default_files" || -z "$e2e_files" ]]; then
    echo "test_integration.sh: empty Go package metadata; refusing an incomplete partition" >&2
    return 1
  fi
  selected="$(awk '
    NR == FNR {
      covered[$1] = 1
      for (i = 2; i <= NF; i++) files[$1 SUBSEP $i] = 1
      next
    }
    NF {
      keep = !covered[$1]
      for (i = 2; i <= NF; i++) if (!(($1 SUBSEP $i) in files)) keep = 1
      if (keep) print $1
    }
  ' <(printf '%s\n' "$e2e_files") <(printf '%s\n' "$default_files") | sort -u)"
  if [[ -z "$selected" ]]; then
    echo "test_integration.sh: no Checks packages remain; refusing to test nothing" >&2
    return 1
  fi
  printf '%s\n' "$selected"
}

# These modes only inspect source/Go metadata, before any dependency setup.
case "${1:-}" in
  --list-integration-packages) selected_integration_packages; exit ;;
  --list-checks-packages) checks_packages; exit ;;
esac

# Package processes own separate PostgreSQL databases. Parallel processes must
# also own separate Redis servers: some recovery tests intentionally FLUSHALL.
for arg in "$@"; do
  case "$arg" in
    -p|--p|-p=*|--p=*|-parallel|--parallel|-parallel=*|--parallel=*|-test.parallel|--test.parallel|-test.parallel=*|--test.parallel=*)
      echo "Use OPENRAILS_TEST_PACKAGES for package concurrency; tests within each package stay serial" >&2
      exit 2
      ;;
  esac
done
package_parallelism="${OPENRAILS_TEST_PACKAGES:-1}"
if [[ ! "$package_parallelism" =~ ^[1-9][0-9]*$ ]]; then
  echo "OPENRAILS_TEST_PACKAGES must be a positive integer" >&2
  exit 2
fi
if [[ "$package_parallelism" -gt 1 && -n "${OPENRAILS_TEST_REDIS_ADDR:-}" ]]; then
  echo "Parallel packages require process-owned Redis; unset OPENRAILS_TEST_REDIS_ADDR or use OPENRAILS_TEST_PACKAGES=1" >&2
  exit 2
fi

if [ "$#" -eq 0 ]; then
  set -- ./...
fi

args=()
for arg in "$@"; do
  if [ "$arg" = "./..." ]; then
    pkgs=()
    package_list="$(selected_integration_packages)"
    # Start the long workflow packages before the short ones so their worker
    # and browser waits overlap useful work instead of extending the run's tail.
    package_list="$(printf '%s\n' "$package_list" | awk '
      $0 == "./internal/integrationharness" { priority = 0 }
      $0 == "./internal/app" { priority = 1 }
      $0 == "./internal/river" { priority = 2 }
      $0 == "./embed" { priority = 3 }
      { print (priority == "" ? 4 : priority), $0; priority = "" }
    ' | sort -k1,1n -k2,2 | cut -d" " -f2-)"
    while IFS= read -r pkg; do
      [ -n "$pkg" ] && pkgs+=("$pkg")
    done <<< "$package_list"
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

if [[ "$package_parallelism" -eq 1 && -z "${OPENRAILS_TEST_REDIS_ADDR:-}" ]]; then
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
if [[ "$package_parallelism" -eq 1 ]]; then
  export OPENRAILS_TEST_REDIS_ADDR="${OPENRAILS_TEST_REDIS_ADDR:-127.0.0.1:${GARNET_HOST_PORT}}"
fi

# -count=1 is MANDATORY, not stylistic. Go's test-result cache keys on package
# content, env vars and files read — it cannot see the Postgres/Garnet stack
# these tests actually exercise, so a cached `ok` is a pass that was never
# re-earned against the current schema and data. Observed live (or#855): with a
# warm GOCACHE, whole integration packages came back `ok … (cached)` without a
# single query running.
go test -vet=all -race -count=1 -p "$package_parallelism" -parallel 1 -tags=integration -timeout "${OPENRAILS_INTEGRATION_TIMEOUT:-25m}" "${args[@]}"
