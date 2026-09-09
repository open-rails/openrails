#!/usr/bin/env bash
set -euo pipefail

# The guard is implemented as a Go test so it runs on stock macOS Bash 3.2 and
# automatically participates in `go test ./...`. An optional root preserves the
# fixture/testing entrypoint used by tooling.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
guard_root="$(cd "${1:-${repo_root}}" && pwd)"
export OPENRAILS_BUSINESS_TIME_ROOT="${guard_root}"
cd "${repo_root}"
go test ./tools/businesstime -run '^TestRepositoryBusinessTimeGuard$' -count=1
echo "business-time guardrail passed"
