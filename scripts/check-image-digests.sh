#!/usr/bin/env bash
# Every image CI pulls is pinned by @sha256 digest, so a moved tag cannot change
# what a job runs: workflow and action image: keys and image= options (an
# ${{ env.NAME }} through NAME's definition in the same file), docker:// actions,
# and the images scripts/ci-services.sh pulls.
set -euo pipefail
cd "$(dirname "$0")/.."
status=0
check() {
  local file=$1 ref=$2 name
  ref="${ref%%#*}"
  ref="$(sed -E 's/^[[:space:]"'"'"']+|[[:space:]"'"'"']+$//g' <<<"$ref")"
  if [[ $ref =~ ^\$\{\{[[:space:]]*env\.([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*\}\}$ ]]; then
    name="${BASH_REMATCH[1]}"
    ref="$(sed -nE "s/^[[:space:]]*${name}:[[:space:]]*([^[:space:]#]+).*/\1/p" "$file" | head -n 1)"
  fi
  if [[ $ref != *@sha256:* ]]; then
    printf '%s: image %s is not pinned by @sha256 digest\n' "$file" "${ref:-(unset)}" >&2
    status=1
  fi
}
for file in .github/workflows/*.y*ml .github/actions/*/action.y*ml; do
  [[ -e $file ]] || continue
  while IFS= read -r ref; do check "$file" "$ref"; done < <(
    grep -oP '^\s*(-\s+)?image:\s*\K.+' "$file" || true
    grep -oP '\bimage=\K(\$\{\{[^}]*\}\}|[^,\s]+)' "$file" || true
    grep -oP '\buses:\s*docker://\K\S+' "$file" || true
  )
done
while IFS= read -r ref; do check scripts/ci-services.sh "$ref"; done < <(grep -oP 'ci-pull-image\.sh\s+\K[^)\s]+' scripts/ci-services.sh || true)
exit "$status"
