#!/usr/bin/env bash
# Build the merchant admin console SPA (web/admin, #754).
#
#   in-repo:   bash scripts/build-admin-console.sh   (builds web/admin/dist,
#              which web/admin/embed.go go:embeds into the binary)
#   consumer:  bash "$(go list -m -f '{{.Dir}}' github.com/open-rails/openrails)/scripts/build-admin-console.sh" internal/webassets/dist
#   with a host's extensions (docs/admin-console.md, "Extending the console"):
#              bash .../build-admin-console.sh --extensions web/console/index.ts --extensions-src web/src internal/webassets/dist
#
# The Go module cache is read-only, so when the SPA source isn't writable it is
# copied to a temp dir before `pnpm install` (pnpm must write node_modules). A
# build with extensions always uses such a copy: it rewrites
# src/extensions/host.ts to re-export the host's module.
set -euo pipefail

usage() {
  echo "usage: $0 [--extensions <module> [--extensions-src <dir>]] [output-dir]" >&2
  exit 2
}

extensions=""
extensions_src=""
args=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --extensions) [[ $# -ge 2 ]] || usage; extensions="$2"; shift 2 ;;
    --extensions-src) [[ $# -ge 2 ]] || usage; extensions_src="$2"; shift 2 ;;
    -*) usage ;;
    *) args+=("$1"); shift ;;
  esac
done
[[ ${#args[@]} -le 1 ]] || usage
[[ -n "$extensions" || -z "$extensions_src" ]] || usage
if [[ -n "$extensions" ]]; then
  [[ -f "$extensions" ]] || { echo "error: --extensions $extensions is not a file" >&2; exit 2; }
  extensions="$(cd "$(dirname "$extensions")" && pwd)/$(basename "$extensions")"
fi
if [[ -n "$extensions_src" ]]; then
  [[ -d "$extensions_src" ]] || { echo "error: --extensions-src $extensions_src is not a directory" >&2; exit 2; }
  extensions_src="$(cd "$extensions_src" && pwd)"
fi
set -- "${args[@]+"${args[@]}"}"

command -v pnpm >/dev/null 2>&1 || {
  echo "error: pnpm is required to build the admin console (Node is a BUILD-time dependency only, #754)" >&2
  exit 1
}

src="$(cd "$(dirname "${BASH_SOURCE[0]}")/../web/admin" && pwd)"
out="$(mkdir -p "${1:-$src/dist}" && cd "${1:-$src/dist}" && pwd)"

build_dir="$src"
if [[ ! -w "$src" || -n "$extensions" ]]; then
  [[ $# -eq 1 ]] || { echo "error: pass an output dir (the source is read-only or this build has extensions)" >&2; exit 2; }
  build_dir="$(mktemp -d)"
  trap 'rm -rf "$build_dir"' EXIT
  tar -C "$src" --exclude=./node_modules --exclude=./console-host.json -cf - . | tar -C "$build_dir" -xf -
  chmod -R u+w "$build_dir"
fi

cd "$build_dir"
pnpm install --frozen-lockfile --ignore-scripts
if [[ -n "$extensions" ]]; then
  # The host type-checks its own extension code; the console's own sources are
  # checked in this repo's CI, so the host build is Vite's alone.
  node -e 'const [module, src] = process.argv.slice(1)
    require("fs").writeFileSync("src/extensions/host.ts", `export { default } from ${JSON.stringify(module)}\n`)
    const sources = [require("path").dirname(module), src].filter(Boolean)
    require("fs").writeFileSync("console-host.json", JSON.stringify({ src, sources }) + "\n")' "$extensions" "$extensions_src"
  ./node_modules/.bin/vite build --outDir "$out" --emptyOutDir
else
  pnpm run build --outDir "$out" --emptyOutDir
fi
test -f "$out/index.html"
echo "admin console built: $out"
