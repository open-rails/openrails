#!/usr/bin/env bash
# Focused local adapter qualification; root Checks/E2E already include these packages.
# Optional Go build flags (for example -tags=integration) apply to vet and tests.
set -euo pipefail
cd "$(dirname "$0")/.."
bash scripts/check-embedded-auth-boundary.sh
go test -vet=all -race -count=1 "$@" ./adapters/gin/... ./adapters/fiber/...
