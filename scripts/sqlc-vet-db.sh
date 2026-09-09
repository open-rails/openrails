#!/usr/bin/env bash
# (Re)creates the throwaway database that `sqlc vet` (db-prepare rule)
# PREPAREs every query against. Built fresh from the repo's own migrations so
# vet always checks against exactly what migrations/ defines — never against a
# possibly-drifted dev volume.
#
#   SQLC_ADMIN_DATABASE_URL  admin connection used to drop/create the vet DB
#                            (default: local docker-compose postgres on 5434)
#   SQLC_POSTGRES_HOST       default admin host (default: 127.0.0.1)
#   POSTGRES_HOST_PORT       default admin port (default: 5434)
#   SQLC_VET_DB              vet database name (default: openrails_sqlc_vet)
#
# Prints the vet database URL on stdout (everything else goes to stderr), so:
#   export SQLC_DATABASE_URL="$(scripts/sqlc-vet-db.sh)"
set -euo pipefail
cd "$(dirname "$0")/.."

POSTGRES_HOST="${SQLC_POSTGRES_HOST:-127.0.0.1}"
POSTGRES_PORT="${POSTGRES_HOST_PORT:-5434}"
ADMIN_URL="${SQLC_ADMIN_DATABASE_URL:-postgres://admin:admin_password@${POSTGRES_HOST}:${POSTGRES_PORT}/openrails_db?sslmode=disable}"
VET_DB="${SQLC_VET_DB:-openrails_sqlc_vet}"

use_compose_psql=false
if ! command -v psql >/dev/null 2>&1; then
    command -v docker >/dev/null 2>&1 || {
        echo "sqlc-vet-db: neither host psql nor docker is available" >&2
        exit 1
    }
    echo "sqlc-vet-db: host psql not found; using the Compose postgres client" >&2
    docker compose -f docker-compose.yaml up -d --wait postgres 1>&2
    use_compose_psql=true
fi

url_database() {
    printf '%s' "$1" | sed -E 's|postgres(ql)?://[^/]+/([^?]+).*|\2|'
}

url_user() {
    printf '%s' "$1" | sed -E 's|postgres(ql)?://([^:/@]+).*|\2|'
}

psql_command() {
    local url="$1"
    shift
    if [ "$use_compose_psql" = true ]; then
        docker compose -f docker-compose.yaml exec -T postgres \
            psql -U "$(url_user "$url")" -d "$(url_database "$url")" "$@"
    else
        psql "$url" "$@"
    fi
}

psql_file() {
    local url="$1"
    local file="$2"
    shift 2
    if [ "$use_compose_psql" = true ]; then
        docker compose -f docker-compose.yaml exec -T postgres \
            psql -U "$(url_user "$url")" -d "$(url_database "$url")" \
            -v ON_ERROR_STOP=1 -q "$@" <"$file"
    else
        psql "$url" -v ON_ERROR_STOP=1 -q "$@" -f "$file"
    fi
}

psql_command "$ADMIN_URL" -v ON_ERROR_STOP=1 -q \
    -c "DROP DATABASE IF EXISTS ${VET_DB}" \
    -c "CREATE DATABASE ${VET_DB}" 1>&2

# Swap the database name in the admin URL.
VET_URL="$(printf '%s' "$ADMIN_URL" | sed -E "s|(postgres(ql)?://[^/]+/)[^?]+|\1${VET_DB}|")"

for f in migrations/bootstrap/*.sql; do
    psql_file "$VET_URL" "$f" 1>&2
done
# profiles_shim stands in for AuthKit's own migrations, which create the
# `profiles` schema FIRST in a real deploy. It must load BEFORE the openrails
# migrations, because 0007+ GRANT on schema profiles — loading it afterwards
# fails the whole build at 0007 with "schema profiles does not exist".
psql_file "$VET_URL" internal/db/schema/profiles_shim.sql 1>&2
# Migration prefixes are fixed-width, so byte-order is version-order and works
# on both GNU and macOS/BSD sort (which has no -V flag).
for f in $(ls migrations/postgres/*.up.sql | LC_ALL=C sort); do
    psql_file "$VET_URL" "$f" -1 1>&2
done

printf '%s\n' "$VET_URL"
