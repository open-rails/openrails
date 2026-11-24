#!/usr/bin/env bash
# Build the SDK once, then both embedded applications and the npm archive from
# that same frozen checkout. GoReleaser must never ship a console-only binary.
set -euo pipefail
[[ $# -eq 2 ]] || { echo 'usage: build-release-assets.sh <version> <archive-dir>' >&2; exit 2; }
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
git diff --quiet HEAD -- web || {
  echo 'release frontend sources must be a frozen checkout' >&2; exit 1;
}
run=(pnpm --config.verify-deps-before-run=false run)
(cd web/billing-ui && pnpm install --frozen-lockfile --ignore-scripts && "${run[@]}" build)
for app in web/console web/pay; do
  # Refresh the file: SDK dependency, even when package.json still says 0.0.0.
  # This reuses the SDK just built above, not an older copied dependency.
  (cd "$app" && pnpm install --force --frozen-lockfile --ignore-scripts && "${run[@]}" build)
  test -s "$app/dist/index.html"
done
git diff --quiet HEAD -- web
# Pack that SDK build as <archive-dir>/openrails-billing-ui-<version>.tgz for release.yaml's npm publish.
version="${1#v}"
out="$(mkdir -p "$2" && cd "$2" && pwd)"
cd web/billing-ui
for entry in index.js client.js admin.js index.d.ts; do test -s "dist/$entry"; done
backup="$(mktemp)"
cp package.json "$backup"
trap 'cp "$backup" package.json; rm -f "$backup"' EXIT
npm version "$version" --no-git-tag-version --allow-same-version >/dev/null
pnpm pack --pack-destination "$out"
tar -tzf "$out/openrails-billing-ui-$version.tgz" package/dist/index.js >/dev/null
