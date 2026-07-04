#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -eq 0 ]; then
  set -- ./...
fi

export POSTGRES_HOST_PORT="${POSTGRES_HOST_PORT:-5434}"
export GARNET_HOST_PORT="${GARNET_HOST_PORT:-6380}"

docker compose -f docker-compose.yaml up -d --wait postgres garnet

export OPENRAILS_TEST_DB_DSN="${OPENRAILS_TEST_DB_DSN:-postgresql://admin:admin_password@127.0.0.1:${POSTGRES_HOST_PORT}/openrails_db?sslmode=disable}"
export OPENRAILS_TEST_REDIS_ADDR="${OPENRAILS_TEST_REDIS_ADDR:-127.0.0.1:${GARNET_HOST_PORT}}"

go test -p 1 -parallel 1 -tags=integration -timeout "${OPENRAILS_INTEGRATION_TIMEOUT:-25m}" "$@"
