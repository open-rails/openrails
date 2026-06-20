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

psql "$ADMIN_URL" -v ON_ERROR_STOP=1 -q \
    -c "DROP DATABASE IF EXISTS ${VET_DB}" \
    -c "CREATE DATABASE ${VET_DB}" 1>&2

# Swap the database name in the admin URL.
VET_URL="$(printf '%s' "$ADMIN_URL" | sed -E "s|(postgres(ql)?://[^/]+/)[^?]+|\1${VET_DB}|")"

for f in migrations/bootstrap/*.sql; do
    psql "$VET_URL" -v ON_ERROR_STOP=1 -q -f "$f" 1>&2
done
for f in $(ls migrations/postgres/*.up.sql | sort -V); do
    psql "$VET_URL" -v ON_ERROR_STOP=1 -q -f "$f" 1>&2
done
psql "$VET_URL" -v ON_ERROR_STOP=1 -q -f internal/db/schema/profiles_shim.sql 1>&2

printf '%s\n' "$VET_URL"
