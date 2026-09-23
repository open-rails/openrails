#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
: "${OPENRAILS_GREENFIELD_DSN:?Set OPENRAILS_GREENFIELD_DSN to a disposable PostgreSQL database}"

go test -vet=all -race -count=1 -p 1 -parallel 1 \
  -tags='greenfield,integration' \
  -timeout "${OPENRAILS_GREENFIELD_TIMEOUT:-8m}" \
  ./ci/greenfield
