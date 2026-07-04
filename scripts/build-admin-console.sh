#!/usr/bin/env bash
# Build the merchant admin console SPA (web/admin) into <output-dir> (#754).
#
# Works in-repo AND from a module consumer (module-cache files are read-only
# and NOT executable — invoke via bash):
#   in-repo:   ./scripts/build-admin-console.sh cmd/openrails/consoleassets/dist
#   consumer:  bash "$(go list -m -f '{{.Dir}}' github.com/open-rails/openrails)/scripts/build-admin-console.sh" internal/webassets/dist
#
# The Go module cache is read-only, so when the SPA source isn't writable it is
# copied to a temp dir before `npm ci` (npm must write node_modules).
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <output-dir>" >&2
  exit 2
fi

command -v npm >/dev/null 2>&1 || {
  echo "error: npm is required to build the admin console (Node is a BUILD-time dependency only, #754)" >&2
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
npm ci
npm run build -- --outDir "$out" --emptyOutDir
echo "admin console built: $out"
