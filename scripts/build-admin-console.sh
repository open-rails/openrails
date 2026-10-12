#!/usr/bin/env bash
# Build the standalone console from the public BillingAdmin component.
set -euo pipefail
[[ $# -le 1 && ${1:-} != -* ]] || { echo "usage: $0 [output-dir]" >&2; exit 2; }
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$(mkdir -p "${1:-$root/server/web/console/dist}" && cd "${1:-$root/server/web/console/dist}" && pwd)"
run=(pnpm --config.verify-deps-before-run=false run)
pnpm --dir "$root/sdk/billing-ui" install --frozen-lockfile --ignore-scripts
(cd "$root/sdk/billing-ui" && "${run[@]}" build)
pnpm --dir "$root/server/web/console" install --frozen-lockfile --ignore-scripts
(cd "$root/server/web/console" && "${run[@]}" build --outDir "$out" --emptyOutDir)
test -f "$out/index.html"
echo "admin console built: $out"
