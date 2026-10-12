#!/usr/bin/env bash
# CI's disposable services for the end-to-end jobs, pulled and started in the
# background while the job sets up Go:
#   scripts/ci-services.sh start postgres [redis] [vault]
#   ...
#   scripts/ci-services.sh ready
# ready waits until every started service answers, prints their logs, and adds
# the variables they publish to $GITHUB_ENV. The fixed addresses
# (OPENRAILS_E2E_DSN and the like) come from the job's env.
set -euo pipefail
cd "$(dirname "$0")/.."
state="${RUNNER_TEMP:?}/ci-services"

start_postgres() {
  local image
  image="$(scripts/ci-pull-image.sh postgres:18-alpine)"
  # The multi-replica fleets (#1075) need more than the default 100 connections.
  docker run -d --pull never --name e2e-postgres -p 5432:5432 \
    --tmpfs /var/lib/postgresql:rw,size=2g \
    -e POSTGRES_DB=openrails_test -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres \
    "$image" -c max_connections=500 -c max_locks_per_transaction=256
  timeout 60 bash -c 'until docker exec e2e-postgres pg_isready -h 127.0.0.1 -U postgres -d openrails_test; do sleep 1; done'
}

start_redis() {
  local image url
  image="$(scripts/ci-pull-image.sh redis:7-alpine)"
  docker run -d --pull never --name e2e-redis -p 6379:6379 "$image"
  # One the outage test pauses and resumes.
  docker run -d --pull never --name "${OPENRAILS_E2E_REDIS_OUTAGE_CONTAINER:-e2e-redis-outage}" -p 127.0.0.1:6381:6379 "$image"
  # Another takes only TLS and an ACL user.
  url="$(scripts/e2e-redis-tls.sh "$state/redis-tls" 6380 "$image")"
  printf 'OPENRAILS_E2E_REDIS_TLS_URL=%s\nOPENRAILS_E2E_REDIS_TLS_CA=%s\n' "$url" "$state/redis-tls/ca.pem" >"$state/redis.env"
}

# A dev server: in memory, unsealed, OPENRAILS_E2E_VAULT_TOKEN its root token.
# Each test mounts its own KV v2 engine (internal/vaulttest).
start_vault() {
  local image
  image="$(scripts/ci-pull-image.sh hashicorp/vault:2.1.1)"
  docker run -d --pull never --name e2e-vault -p 8200:8200 \
    -e VAULT_DEV_ROOT_TOKEN_ID="${OPENRAILS_E2E_VAULT_TOKEN:?}" "$image"
  timeout 60 bash -c 'until curl -fsS http://127.0.0.1:8200/v1/sys/health >/dev/null; do sleep 1; done'
}

case "${1:-}" in
start)
  shift
  (($#)) || { echo "usage: ci-services.sh start postgres|redis|vault..." >&2; exit 2; }
  rm -rf "$state"
  mkdir -p "$state"
  printf '%s\n' "$@" >"$state/services"
  for service in "$@"; do
    case "$service" in postgres | redis | vault) ;; *) echo "ci-services: unknown service $service" >&2; exit 2 ;; esac
    # Detached from the step's output: its log and exit status land in $state.
    (
      set +e
      (set -e; "start_$service") >"$state/$service.log" 2>&1
      echo "$?" >"$state/$service.status.tmp"
      mv "$state/$service.status.tmp" "$state/$service.status"
    ) </dev/null >/dev/null 2>&1 &
  done
  ;;
ready)
  mapfile -t services <"$state/services"
  failed=0
  for service in "${services[@]}"; do
    deadline=$((SECONDS + 300))
    until [[ -f "$state/$service.status" ]]; do
      if ((SECONDS >= deadline)); then
        echo "::error::$service did not start within 300s"
        cat "$state/$service.log"
        exit 1
      fi
      sleep 1
    done
    echo "::group::$service"
    cat "$state/$service.log"
    echo "::endgroup::"
    if [[ "$(<"$state/$service.status")" != 0 ]]; then
      echo "::error::$service failed to start (exit $(<"$state/$service.status"))"
      failed=1
    fi
  done
  ((failed == 0))
  if compgen -G "$state/*.env" >/dev/null; then
    cat "$state"/*.env >>"${GITHUB_ENV:?}"
  fi
  ;;
*)
  echo "usage: ci-services.sh start postgres|redis|vault... | ready" >&2
  exit 2
  ;;
esac
