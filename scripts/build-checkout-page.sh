#!/usr/bin/env bash
# Build the hosted checkout page (web/checkout), and first the billing-ui it
# is built from (a path dependency).
#
#   in-repo:   bash scripts/build-checkout-page.sh   (builds web/checkout/dist,
#              which web/checkout/embed.go go:embeds into the binary)
#   consumer:  bash "$(go list -m -f '{{.Dir}}' github.com/open-rails/openrails)/scripts/build-checkout-page.sh" <output-dir>
#              and pass the build as server.Deps.CheckoutAssets
#
# The Go module cache is read-only, so when the sources aren't writable both
# are copied to a temp dir, keeping their relative layout, before building.
set -euo pipefail

command -v pnpm >/dev/null 2>&1 || {
  echo "error: pnpm is required to build the checkout page (Node is a BUILD-time dependency only)" >&2
  exit 1
}

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$(mkdir -p "${1:-$root/web/checkout/dist}" && cd "${1:-$root/web/checkout/dist}" && pwd)"

build_root="$root"
if [[ ! -w "$root/web/checkout" ]]; then
  [[ $# -eq 1 ]] || { echo "error: pass an output dir (the source is read-only)" >&2; exit 2; }
  build_root="$(mktemp -d)"
  trap 'rm -rf "$build_root"' EXIT
  for dir in sdk/billing-ui web/checkout; do
    mkdir -p "$build_root/$dir"
    find "$root/$dir" -mindepth 1 -maxdepth 1 ! -name node_modules ! -name dist -exec cp -R {} "$build_root/$dir/" \;
  done
  chmod -R u+w "$build_root"
fi

# install just ran with --frozen-lockfile; pnpm's own pre-run check is redundant
# and crashes some pnpm 11 releases ("currentPnpmfiles is not iterable").
run=(pnpm --config.verify-deps-before-run=false run)
(cd "$build_root/sdk/billing-ui" && pnpm install --frozen-lockfile --ignore-scripts && "${run[@]}" build)
cd "$build_root/web/checkout"
pnpm install --frozen-lockfile --ignore-scripts
"${run[@]}" build --outDir "$out" --emptyOutDir
test -f "$out/index.html"
echo "checkout page built: $out"
