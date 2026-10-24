#!/usr/bin/env bash
# Adapter behavior belongs to the named public critical journeys.
set -euo pipefail
cd "$(dirname "$0")/.."
bash scripts/check-embedded-auth-boundary.sh
GOWORK=off go build ./adapters/gin/... ./adapters/fiber/...
(cd adapters/authkit && GOWORK=off go build ./...)
