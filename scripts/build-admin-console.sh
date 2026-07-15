#!/usr/bin/env bash
# Build the merchant admin console SPA (web/admin) into <output-dir> (#754).
#
# Works in-repo AND from a module consumer:
#   in-repo:   ./scripts/build-admin-console.sh cmd/openrails/consoleassets/dist
#   consumer:  "$(go list -m -f '{{.Dir}}' github.com/open-rails/openrails)/scripts/build-admin-console.sh" internal/webassets/dist
#
# The Go module cache is read-only, so when the SPA source isn't writable it is
# copied to a temp dir before `pnpm install` (pnpm must write node_modules).
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <output-dir>" >&2
  exit 2
fi

command -v pnpm >/dev/null 2>&1 || {
  echo "error: pnpm is required to build the admin console (Node is a BUILD-time dependency only, #754)" >&2
  exit 1
}

out="$(mkdir -p "$1" && cd "$1" && pwd)"
src="$(cd "$(dirname "${BASH_SOURCE[0]}")/../web/admin" && pwd)"

build_dir="$src"
if [[ ! -w "$src" ]]; then
  build_dir="$(mktemp -d)"
  trap 'rm -rf "$build_dir"' EXIT
  cp -R "$src/." "$build_dir/"
  chmod -R u+w "$build_dir"
fi

cd "$build_dir"
pnpm install --frozen-lockfile --ignore-scripts
pnpm run build --outDir "$out" --emptyOutDir
echo "admin console built: $out"
