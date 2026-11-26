#!/usr/bin/env bash
# Released migrations are immutable: every migration file the latest release
# shipped is byte-identical here. A schema change is a new migration.
set -euo pipefail
cd "$(dirname "$0")/.."

dir=internal/migrate/postgres
# 0.284.0's baseline replaced every earlier chain; older releases are not compared.
first=v0.284.0

latest="$(git ls-remote --tags --refs origin 'refs/tags/v*' | sed 's#.*refs/tags/##' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1)"
if [[ -z "$latest" || "$(printf '%s\n%s\n' "$first" "$latest" | sort -V | head -1)" != "$first" ]]; then
    echo "No release from $first on; nothing to compare."
    exit 0
fi
git fetch --quiet --no-tags --depth=1 origin "refs/tags/$latest:refs/tags/$latest"
# Added files are new migrations; anything else touches a released one.
changed="$(git diff --name-only --diff-filter=a "$latest" HEAD -- "$dir")"
if [[ -n "$changed" ]]; then
    echo "released migrations are immutable; add a new migration. Changed since $latest:" >&2
    echo "$changed" >&2
    exit 1
fi
echo "The migrations released in $latest are unchanged."
