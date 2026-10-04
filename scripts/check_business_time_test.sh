#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"
go test ./internal/businesstime -count=1
echo "business-time guardrail regression tests passed"
