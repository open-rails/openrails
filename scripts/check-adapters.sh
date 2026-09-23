#!/usr/bin/env bash
# Test native adapters against this exact core source without published replace directives.
# Optional Go build flags (for example -tags=integration) apply to vet and tests.
set -euo pipefail
cd "$(dirname "$0")/.."
adapter_workspace="$(mktemp -d)"
trap 'rm -rf "$adapter_workspace"' EXIT
export GOWORK="$adapter_workspace/go.work"
go work init "$PWD" "$PWD/adapters/gin" "$PWD/adapters/fiber"
bash scripts/check-embedded-auth-boundary.sh --adapters
go test -vet=all -race -count=1 "$@" ./adapters/gin/... ./adapters/fiber/...
