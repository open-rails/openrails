#!/usr/bin/env bash
set -euo pipefail

# Packages containing files (test or non-test) that carry the `integration`
# build tag. These packages run their unit and integration tests together;
# Checks selects the remaining packages, preserving default-only variants.
# `./...` (and no args) expands to this set; explicit arguments pass through.
integration_packages() {
  # All library packages, including native adapters, share the root module.
  find . -mindepth 1 \
    \( -type d \( -name .git -o -name node_modules -o -name vendor \) -prune \) -o \
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
run_test_command() {
  local output="$1"
  shift
  if "$@" >"$output" 2>&1; then
    echo 0 >"$output.status"
  else
    echo $? >"$output.status"
  fi
}

# The integration harness is intentionally one Go package, but it contains a
# large collection of independent top-level workflows. Running that package as
# one process means one stalled provider workflow holds the whole E2E job until
# the package timeout. When explicitly enabled, run its top-level tests in
# separate processes. dbtest creates a fresh PostgreSQL database per process
# when OPENRAILS_TEST_DB_DSN is supplied, and each process gets its own Redis
# container when OPENRAILS_TEST_REDIS_ADDR is unset. This keeps the existing
# isolation contract while allowing the workflow to make progress around a
# single slow/failing scenario.
run_harness_shards() {
  local shard_count="${OPENRAILS_TEST_HARNESS_SHARDS:-0}"
  [[ "$shard_count" =~ ^[1-9][0-9]*$ ]] || return 1
  (( shard_count > 1 )) || return 1

  local harness="./internal/integrationharness"
  local json_output=0
  local -a common_args=()
  local arg
  for arg in "${args[@]}"; do
    case "$arg" in
      "$harness"|github.com/open-rails/openrails/internal/integrationharness)
        ;;
      -json)
        json_output=1
        ;;
      *)
        common_args+=("$arg")
        ;;
    esac
  done

  local -a names=()
  while IFS= read -r arg; do
    [[ "$arg" =~ ^Test[A-Za-z0-9_]+$ ]] && names+=("$arg")
  done < <(go test -vet=all -race -count=1 -parallel 1 "${common_args[@]}" -list '^Test' "$harness")
  (( ${#names[@]} > 0 )) || {
    echo "test_integration.sh: harness sharding found no top-level tests" >&2
    return 1
  }

  local shard_dir
  shard_dir="$(mktemp -d "${TMPDIR:-/tmp}/openrails-integration-shards.XXXXXX")"
  local -a pids=()
  local shard regex index
  for (( shard = 0; shard < shard_count; shard++ )); do
    regex='^('
    index=0
    for arg in "${names[@]}"; do
      if (( index % shard_count == shard )); then
        [[ "$regex" == '^(' ]] || regex+='|'
        regex+="$arg"
      fi
      ((index++))
    done
    regex+=')$'
    [[ "$regex" != '^()$' ]] || continue
    local -a command=(go test -vet=all -race -count=1 -parallel 1 -p 1 "${common_args[@]}" -run "$regex")
    (( json_output )) && command+=( -json )
    command+=( "$harness" )
    run_test_command "$shard_dir/shard-$shard.out" "${command[@]}" &
    pids+=("$!")
  done

  local status=0 pid code
  for pid in "${pids[@]}"; do
    wait "$pid" || true
  done
  for (( shard = 0; shard < shard_count; shard++ )); do
    [[ -f "$shard_dir/shard-$shard.out" ]] || continue
    cat "$shard_dir/shard-$shard.out"
    code="$(cat "$shard_dir/shard-$shard.out.status")"
    [[ "$code" == 0 ]] || status=1
  done
  rm -rf "$shard_dir"
  return "$status"
}

harness_shards="${OPENRAILS_TEST_HARNESS_SHARDS:-0}"
if [[ "$harness_shards" =~ ^[1-9][0-9]*$ ]] && (( harness_shards > 1 )) &&
   printf '%s\n' "${args[@]}" | grep -Fxq './internal/integrationharness'; then
  # Keep the ordinary package command free of the sharded package. Its result
  # and each shard's result are concatenated so -json consumers see one stream.
  ordinary_args=()
  for arg in "${args[@]}"; do
    [[ "$arg" == './internal/integrationharness' || "$arg" == 'github.com/open-rails/openrails/internal/integrationharness' ]] || ordinary_args+=("$arg")
  done
  shard_dir="$(mktemp -d "${TMPDIR:-/tmp}/openrails-integration-run.XXXXXX")"
  run_test_command "$shard_dir/ordinary.out" go test -vet=all -race -count=1 -p "$package_parallelism" -parallel 1 -tags=integration -timeout "${OPENRAILS_INTEGRATION_TIMEOUT:-25m}" "${ordinary_args[@]}" &
  ordinary_pid=$!
  if run_harness_shards; then
    harness_status=0
  else
    harness_status=$?
  fi
  wait "$ordinary_pid" || true
  cat "$shard_dir/ordinary.out"
  ordinary_status="$(cat "$shard_dir/ordinary.out.status")"
  rm -rf "$shard_dir"
  [[ "$ordinary_status" == 0 && "$harness_status" == 0 ]]
else
  go test -vet=all -race -count=1 -p "$package_parallelism" -parallel 1 -tags=integration -timeout "${OPENRAILS_INTEGRATION_TIMEOUT:-25m}" "${args[@]}"
fi
