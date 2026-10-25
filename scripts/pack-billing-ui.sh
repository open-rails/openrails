#!/usr/bin/env bash
# Pack @openrails/billing-ui as <out-dir>/openrails-billing-ui-<version>.tgz.
# release.yaml publishes it to npm.
set -euo pipefail

if [[ $# -lt 2 || $# -gt 3 || ( $# -eq 3 && $3 != --prebuilt-sdk ) ]]; then
  echo "usage: $0 <version> <out-dir> [--prebuilt-sdk]" >&2
  exit 2
fi
version="${1#v}"
out="$(mkdir -p "$2" && cd "$2" && pwd)"

cd "$(dirname "${BASH_SOURCE[0]}")/../sdk/billing-ui"
backup="$(mktemp)"
cp package.json "$backup"
trap 'cp "$backup" package.json; rm -f "$backup"' EXIT

if [[ ${3:-} == --prebuilt-sdk ]]; then
  # The release asset hook builds this SDK and its two consumers together.
  # This mode is only a packaging step, never a separate SDK build.
  for entry in index.js client.js admin.js index.d.ts; do test -s "dist/$entry"; done
else
  pnpm install --frozen-lockfile --ignore-scripts
  pnpm build
fi
npm version "$version" --no-git-tag-version --allow-same-version >/dev/null
pnpm pack --pack-destination "$out"
tar -tzf "$out/openrails-billing-ui-$version.tgz" package/dist/index.js >/dev/null
