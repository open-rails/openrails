#!/usr/bin/env bash
set -euo pipefail

# Run the source guard directly; a removed test must not silently pass with
# "no test files". This is a static clock-boundary check, not a behavioral suite.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
guard_root="$(cd "${1:-${repo_root}}" && pwd)"
cd "${repo_root}"
GOWORK=off go run ./scripts/business-time "$guard_root"
echo "business-time guardrail passed"
