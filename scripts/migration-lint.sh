#!/usr/bin/env bash
# or#838: lock-safety lint for NEW migrations (squawk). Frozen history is
# excluded by path in .squawk.toml; anything added from now on must pass.
#
#   SQUAWK_BIN  use an already-installed squawk instead of downloading
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="2.61.0"
BIN="${SQUAWK_BIN:-bin/squawk-${VERSION}}"

if [ ! -x "$BIN" ]; then
    mkdir -p "$(dirname "$BIN")"
    case "$(uname -s)-$(uname -m)" in
        Linux-x86_64) asset=squawk-linux-x64 ;;
        Linux-aarch64) asset=squawk-linux-arm64 ;;
        Darwin-arm64) asset=squawk-darwin-arm64 ;;
        Darwin-x86_64) asset=squawk-darwin-x64 ;;
        *) echo "migration-lint: no squawk build for $(uname -sm); set SQUAWK_BIN" 1>&2; exit 1 ;;
    esac
    echo "migration-lint: fetching squawk ${VERSION}" 1>&2
    curl -fsSL -o "$BIN" "https://github.com/sbdchd/squawk/releases/download/v${VERSION}/${asset}"
    chmod +x "$BIN"
fi

# squawk exits 0 even when it reports issues; count them instead.
count="$("$BIN" --reporter json 'migrations/postgres/*.up.sql' 2>/dev/null | tr ',' '\n' | grep -c '"rule_name"' || true)"
if [ "$count" -gt 0 ]; then
    "$BIN" 'migrations/postgres/*.up.sql' 1>&2
    echo "" 1>&2
    echo "migration-lint: $count issue(s) in new migrations. Fix them, or — if the" 1>&2
    echo "operation is genuinely safe here — record why in" 1>&2
    echo "internal/db/queries/EXEMPTIONS.md and exclude the rule in .squawk.toml." 1>&2
    exit 1
fi
echo "migration-lint: clean"
