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

# Run once and keep BOTH the output and the exit status. The earlier version
# piped a JSON run straight into `grep -c … || true`, which turned every way
# squawk can fail — missing binary, unparseable SQL, a glob matching nothing —
# into "0 issues" and a green gate. A gate that cannot go red is worth less than
# no gate, because it is believed.
set +e
report="$("$BIN" 'migrations/postgres/*.up.sql' 2>&1)"
status=$?
set -e

# squawk's own summary line is the only evidence trusted to mean the run
# happened. Two shapes:
#   Found 0 issues in 19 files
#   Found 6 issues in 1 file (checked 20 source files)
plain="$(printf '%s\n' "$report" | sed -E 's/\x1b\[[0-9;]*m//g')"
summary="$(printf '%s\n' "$plain" | grep -E '^Found [0-9]+ issues? in [0-9]+ files?' || true)"
if [ -z "$summary" ]; then
    printf '%s\n' "$report" 1>&2
    echo "" 1>&2
    echo "migration-lint: squawk did not complete (exit $status) — no summary line." 1>&2
    echo "This is a broken gate, not a clean one; do not treat it as a pass." 1>&2
    exit 1
fi

count="$(printf '%s' "$summary" | sed -E 's/^Found ([0-9]+) issues? in .*/\1/')"
if printf '%s' "$summary" | grep -q 'checked'; then
    checked="$(printf '%s' "$summary" | sed -E 's/.*checked ([0-9]+) source files?\).*/\1/')"
else
    checked="$(printf '%s' "$summary" | sed -E 's/^Found [0-9]+ issues? in ([0-9]+) files?.*/\1/')"
fi

# A glob that stops matching, or an excluded_paths list that swallows every
# file, must not read as a pass.
if [ "$checked" -lt 1 ]; then
    echo "migration-lint: squawk checked 0 files — the glob or excluded_paths in" 1>&2
    echo ".squawk.toml no longer matches any migration. Fix that before trusting" 1>&2
    echo "this gate." 1>&2
    exit 1
fi

# squawk exits 0 with findings and non-zero on a genuine failure; neither is
# trusted on its own, so both funnel into the same refusal.
if [ "$count" -gt 0 ] || [ "$status" -ne 0 ]; then
    printf '%s\n' "$report" 1>&2
    echo "" 1>&2
    echo "migration-lint: $count issue(s) in new migrations. Fix them, or — if the" 1>&2
    echo "operation is genuinely safe here — put a '-- squawk-ignore <rule>' on the" 1>&2
    echo "line above the flagged one with the reason above that, and record it in" 1>&2
    echo "internal/db/queries/EXEMPTIONS.md. Exclude a whole rule in .squawk.toml" 1>&2
    echo "only when it cannot apply to this migrator at all." 1>&2
    exit 1
fi
echo "migration-lint: clean ($checked files checked)"
