#!/usr/bin/env bash
# Build the SDK once, then both embedded applications and the npm archive from
# that same frozen checkout. GoReleaser must never ship a console-only binary.
set -euo pipefail
[[ $# -eq 2 ]] || { echo 'usage: build-release-assets.sh <version> <archive-dir>' >&2; exit 2; }
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
git diff --quiet HEAD -- sdk/billing-ui server/web/console web/pay || {
  echo 'release frontend sources must be a frozen checkout' >&2; exit 1;
}
run=(pnpm --config.verify-deps-before-run=false run)
(cd sdk/billing-ui && pnpm install --frozen-lockfile --ignore-scripts && "${run[@]}" build)
for app in server/web/console web/pay; do
  # Refresh the file: SDK dependency, even when package.json still says 0.0.0.
  # This reuses the SDK just built above, not an older copied dependency.
  (cd "$app" && pnpm install --force --frozen-lockfile --ignore-scripts && "${run[@]}" build)
  test -s "$app/dist/index.html"
done
git diff --quiet HEAD -- sdk/billing-ui server/web/console web/pay
bash scripts/pack-billing-ui.sh "$1" "$2" --prebuilt-sdk
