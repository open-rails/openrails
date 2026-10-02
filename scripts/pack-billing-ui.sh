#!/usr/bin/env bash
# Pack @openrails/billing-ui as <out-dir>/openrails-billing-ui-<version>.tgz.
# Each release attaches it (.goreleaser.yaml); hosts install it by URL, so the
# asset name is a contract.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <version> <out-dir>" >&2
  exit 2
fi
version="${1#v}"
out="$(mkdir -p "$2" && cd "$2" && pwd)"

cd "$(dirname "${BASH_SOURCE[0]}")/../sdk/billing-ui"
backup="$(mktemp)"
cp package.json "$backup"
trap 'cp "$backup" package.json; rm -f "$backup"' EXIT

pnpm install --frozen-lockfile --ignore-scripts
pnpm build
npm version "$version" --no-git-tag-version --allow-same-version >/dev/null
pnpm pack --pack-destination "$out"
tar -tzf "$out/openrails-billing-ui-$version.tgz" package/dist/index.js >/dev/null
