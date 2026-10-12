#!/usr/bin/env bash
# Rewrites ci/e2e-durations.tsv, which scripts/e2e.sh splits the suite by:
# each test's median over the e2e-timings-* artifacts of the last N (default
# 10) successful Validate runs, for the tests the newest of them ran. One run's
# timings swing with its runners' load; the median does not.
#   bash scripts/e2e-durations.sh [N]
set -euo pipefail
export LC_ALL=C

cd "$(dirname "$0")/.."
runs="${1:-10}"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT

mapfile -t ids < <(gh run list --workflow ci.yaml --status success --limit "$runs" --json databaseId --jq '.[].databaseId')
newest=
for id in "${ids[@]}"; do
  gh run download "$id" --pattern 'e2e-timings-*' --dir "$dir/runs/$id" >/dev/null 2>&1 || true
  if compgen -G "$dir/runs/$id/*/*.tsv" >/dev/null; then
    newest="${newest:-$id}"
  else
    echo "e2e-durations: run $id has no timings" >&2
  fi
done
[[ -n "$newest" ]] || { echo "e2e-durations: no timings in the last $runs successful runs" >&2; exit 1; }

cut -f1,2 "$dir/runs/$newest"/*/*.tsv | sort -u >"$dir/current"
cat "$dir"/runs/*/*/*.tsv | sort -t$'\t' -k1,1 -k2,2 -k3,3g |
  awk -F'\t' -v OFS='\t' -v current="$dir/current" '
    BEGIN { while ((getline line < current) > 0) keep[line] = 1 }
    function flush() {
      if (n && (key in keep)) print key, (n % 2 ? v[(n + 1) / 2] : sprintf("%.1f", (v[n / 2] + v[n / 2 + 1]) / 2))
    }
    { k = $1 "\t" $2; if (k != key) { flush(); key = k; n = 0 } v[++n] = $3 }
    END { flush() }' >ci/e2e-durations.tsv
echo "e2e-durations: $(wc -l <ci/e2e-durations.tsv) tests from ${#ids[@]} runs (newest $newest)"
