#!/usr/bin/env bash
# or#838: fail on new raw SQL outside internal/db/gen. Matches pgx's
# pool.Query/QueryRow/Exec(ctx, ...) call shape. Files in
# internal/db/queries/LINT_ALLOWLIST.txt are reviewed exemptions (dynamic-SQL
# builders, DDL/bootstrap, see EXEMPTIONS.md); anything else matching is a
# regression — hand SQL that should have been a sqlc query.
#
# Usage: scripts/sql-lint.sh   (wired as `task sql-lint`)
set -euo pipefail
cd "$(dirname "$0")/.."

ALLOWLIST="internal/db/queries/LINT_ALLOWLIST.txt"
# First-arg shapes: bare ctx, r.Context(), context.Background/TODO(). The
# trailing comma is required — it is what separates SQL calls from Redis
# pipeline flushes (`pipe.Exec(ctx)`), which take no statement.
PATTERN='\.(Query|QueryRow|Exec)\((ctx|r\.Context\(\)|context\.(Background|TODO)\(\)),'

# Hard-excluded: generated code, the test-only DB harness, and the query
# auditor itself (its whole job is talking to pg_catalog).
EXCLUDE_DIRS='internal/db/gen/|internal/dbtest/|internal/db/sqlaudit/'

mapfile -t allowed < <(grep -vE '^\s*(#|$)' "$ALLOWLIST" | awk '{print $2}')

violations=()
while IFS= read -r f; do
  echo "$f" | grep -qE "$EXCLUDE_DIRS" && continue
  [[ "$f" == *_test.go ]] && continue
  hit=0
  for a in "${allowed[@]}"; do
    [[ "$f" == "$a" ]] && hit=1 && break
  done
  [[ "$hit" -eq 1 ]] && continue
  violations+=("$f")
done < <(grep -rlE "$PATTERN" --include='*.go' internal/ pkg/ cmd/ embed/ 2>/dev/null | sort)

stale=()
for a in "${allowed[@]}"; do
  [ -f "$a" ] || { stale+=("$a (file gone)"); continue; }
  grep -qE "$PATTERN" "$a" || stale+=("$a (no raw SQL left)")
done

rc=0
if [ "${#violations[@]}" -gt 0 ]; then
  echo "sql-lint: raw SQL outside internal/db/gen and $ALLOWLIST:" 1>&2
  for f in "${violations[@]}"; do
    echo "  $f" 1>&2
    grep -nE "$PATTERN" "$f" | head -3 | sed 's/^/    /' 1>&2
  done
  echo "" 1>&2
  echo "Port these to sqlc (internal/db/queries/*.sql + gen.Queries). If the SQL is" 1>&2
  echo "genuinely dynamic, add the rationale to internal/db/queries/EXEMPTIONS.md and" 1>&2
  echo "list the file in $ALLOWLIST." 1>&2
  rc=1
fi
if [ "${#stale[@]}" -gt 0 ]; then
  echo "sql-lint: stale allowlist entries — delete them from $ALLOWLIST:" 1>&2
  printf '  %s\n' "${stale[@]}" 1>&2
  rc=1
fi
[ "$rc" -eq 0 ] && echo "sql-lint: clean (${#allowed[@]} reviewed exemptions)"
exit "$rc"
