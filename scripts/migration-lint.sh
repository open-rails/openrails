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

# migratekit wraps every migration in a transaction, so PostgreSQL cannot build
# these indexes CONCURRENTLY. Lint those files separately with only that
# incompatible rule disabled; every other rule remains enforcing.
TRANSACTIONAL_INDEX_CREATE_MIGRATIONS=(
    "migrations/postgres/0011_query_audit_indexes.up.sql"
    "migrations/postgres/0012_rls_index_coverage.up.sql"
)
TRANSACTIONAL_INDEX_REPLACE_MIGRATIONS=(
    "migrations/postgres/0013_total_rail_transaction_uniques.up.sql"
    "migrations/postgres/0015_merchant_permission_group_unique.up.sql"
)
migration_files=()
for file in migrations/postgres/*.up.sql; do
    transactional_index=0
    for indexed_file in "${TRANSACTIONAL_INDEX_CREATE_MIGRATIONS[@]}" "${TRANSACTIONAL_INDEX_REPLACE_MIGRATIONS[@]}"; do
        if [ "$file" = "$indexed_file" ]; then
            transactional_index=1
            break
        fi
    done
    if [ "$transactional_index" -eq 0 ]; then
        migration_files+=("$file")
    fi
done

# squawk exits 0 even when it reports issues; count them instead.
count="$({
    "$BIN" --reporter json "${migration_files[@]}"
    for file in "${TRANSACTIONAL_INDEX_CREATE_MIGRATIONS[@]}"; do
        "$BIN" --reporter json --exclude require-concurrent-index-creation "$file"
    done
    for file in "${TRANSACTIONAL_INDEX_REPLACE_MIGRATIONS[@]}"; do
        "$BIN" --reporter json --exclude require-concurrent-index-creation --exclude require-concurrent-index-deletion "$file"
    done
} 2>/dev/null | tr ',' '\n' | grep -c '"rule_name"' || true)"
if [ "$count" -gt 0 ]; then
    "$BIN" "${migration_files[@]}" 1>&2
    for file in "${TRANSACTIONAL_INDEX_CREATE_MIGRATIONS[@]}"; do
        "$BIN" --exclude require-concurrent-index-creation "$file" 1>&2
    done
    for file in "${TRANSACTIONAL_INDEX_REPLACE_MIGRATIONS[@]}"; do
        "$BIN" --exclude require-concurrent-index-creation --exclude require-concurrent-index-deletion "$file" 1>&2
    done
    echo "" 1>&2
    echo "migration-lint: $count issue(s) in new migrations. Fix them, or — if the" 1>&2
    echo "operation is genuinely safe here — record why in" 1>&2
    echo "internal/db/queries/EXEMPTIONS.md and exclude the rule in .squawk.toml." 1>&2
    exit 1
fi
echo "migration-lint: clean"
