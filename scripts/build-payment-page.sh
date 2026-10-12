#!/usr/bin/env bash
# Build the payment page (web/pay), and first the billing-ui it
# is built from (a path dependency).
#
#   in-repo:   bash scripts/build-payment-page.sh   (builds web/pay/dist,
#              which web/pay/embed.go go:embeds into the binary)
#   consumer:  bash "$(go list -m -f '{{.Dir}}' github.com/open-rails/openrails)/scripts/build-payment-page.sh" <output-dir>
#              and pass the build as server.Deps.PaymentPageAssets
#
# The Go module cache is read-only, so when the sources aren't writable both
# are copied to a temp dir, keeping their relative layout, before building.
set -euo pipefail

command -v pnpm >/dev/null 2>&1 || {
  echo "error: pnpm is required to build the payment page (Node is a BUILD-time dependency only)" >&2
  exit 1
}

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$(mkdir -p "${1:-$root/web/pay/dist}" && cd "${1:-$root/web/pay/dist}" && pwd)"

build_root="$root"
if [[ ! -w "$root/web/pay" ]]; then
  [[ $# -eq 1 ]] || { echo "error: pass an output dir (the source is read-only)" >&2; exit 2; }
  build_root="$(mktemp -d)"
  trap 'rm -rf "$build_root"' EXIT
  for dir in sdk/billing-ui web/pay; do
    mkdir -p "$build_root/$dir"
    find "$root/$dir" -mindepth 1 -maxdepth 1 ! -name node_modules ! -name dist -exec cp -R {} "$build_root/$dir/" \;
  done
  chmod -R u+w "$build_root"
fi

# install just ran with --frozen-lockfile; pnpm's own pre-run check is redundant
# and crashes some pnpm 11 releases ("currentPnpmfiles is not iterable").
run=(pnpm --config.verify-deps-before-run=false run)
(cd "$build_root/sdk/billing-ui" && pnpm install --frozen-lockfile --ignore-scripts && "${run[@]}" build)
cd "$build_root/web/pay"
# A path dependency is copied in at install, and pnpm keeps a copy it
# thinks current: install afresh so the page takes this billing-ui build.
rm -rf node_modules
pnpm install --frozen-lockfile --ignore-scripts
"${run[@]}" build --outDir "$out" --emptyOutDir
test -f "$out/index.html"
echo "payment page built: $out"
