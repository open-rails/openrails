#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -eq 0 ]; then
  set -- ./...
fi

export POSTGRES_HOST_PORT="${POSTGRES_HOST_PORT:-5434}"
export GARNET_HOST_PORT="${GARNET_HOST_PORT:-6380}"
export CLICKHOUSE_KEEPER_HOST_PORT="${CLICKHOUSE_KEEPER_HOST_PORT:-9182}"
export CLICKHOUSE_HTTP_HOST_PORT="${CLICKHOUSE_HTTP_HOST_PORT:-8124}"
export CLICKHOUSE_NATIVE_HOST_PORT="${CLICKHOUSE_NATIVE_HOST_PORT:-9003}"

docker compose -f docker-compose.yaml up -d --wait postgres garnet clickhouse
docker compose -f docker-compose.yaml run --rm --no-deps clickhouse-bootstrap >/dev/null

export OPENRAILS_TEST_DB_DSN="${OPENRAILS_TEST_DB_DSN:-postgresql://admin:admin_password@127.0.0.1:${POSTGRES_HOST_PORT}/openrails_db?sslmode=disable}"
export OPENRAILS_TEST_REDIS_ADDR="${OPENRAILS_TEST_REDIS_ADDR:-127.0.0.1:${GARNET_HOST_PORT}}"
export OPENRAILS_TEST_CH_HTTP_ADDR="${OPENRAILS_TEST_CH_HTTP_ADDR:-http://127.0.0.1:${CLICKHOUSE_HTTP_HOST_PORT}}"
export OPENRAILS_TEST_CH_ADDR="${OPENRAILS_TEST_CH_ADDR:-127.0.0.1:${CLICKHOUSE_NATIVE_HOST_PORT}}"
export OPENRAILS_TEST_CH_NATIVE_ADDR="${OPENRAILS_TEST_CH_NATIVE_ADDR:-127.0.0.1:${CLICKHOUSE_NATIVE_HOST_PORT}}"
export OPENRAILS_TEST_CH_DATABASE="${OPENRAILS_TEST_CH_DATABASE:-analytics}"
export OPENRAILS_TEST_CH_USERNAME="${OPENRAILS_TEST_CH_USERNAME:-analytics_user}"
export OPENRAILS_TEST_CH_PASSWORD="${OPENRAILS_TEST_CH_PASSWORD:-analytics_password}"

go test -p 1 -parallel 1 -tags=integration -timeout "${OPENRAILS_INTEGRATION_TIMEOUT:-25m}" "$@"
