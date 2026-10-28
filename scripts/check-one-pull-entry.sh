#!/usr/bin/env bash
# One pull entry point: only a PSP import proposes from a provider read. The
# reconcile engine is left only in the provider refresh job, which a later
# change deletes.
set -euo pipefail
cd "$(dirname "$0")/.."
fail=0
only() {
  local pattern=$1 allowed=$2 file
  while IFS= read -r file; do
    if [[ $file != "$allowed" ]]; then
      echo "$file: $pattern is called only from $allowed" >&2
      fail=1
    fi
  done < <(git grep -l -E "$pattern" -- '*.go' ':!*_test.go' || true)
}
only 'reconcile\.Propose\(' internal/pspimport/reconcile.go
only 'reconcile\.NewEngine\(' internal/river/jobs_provider_refresh.go
exit "$fail"
