#!/usr/bin/env bash
set -euo pipefail

# Current module repositories live under internal/modules, not internal/db/repo.
# An optional root lets the regression test exercise the real checker in isolation.
guard_root="$(cd "${1:-$(dirname "${BASH_SOURCE[0]}")/..}" && pwd)"
guard_allowlist="${guard_root}/scripts/business-time-allowlist.txt"
[[ -f "${guard_allowlist}" ]] || { echo "missing ${guard_allowlist}" >&2; exit 1; }
guard_paths=()
for dir in internal/modules internal/river internal/http/handlers pkg/service; do
  [[ -d "${guard_root}/${dir}" ]] || { echo "missing scan directory: ${dir}" >&2; exit 1; }
  guard_paths+=("${guard_root}/${dir}")
done

guard_hits="$(mktemp)"
trap 'rm -f "${guard_hits}"' EXIT
status=0
rg -n --no-heading 'time\.Now\(|\bNOW\(\)|CURRENT_TIMESTAMP|clockwork\.NewRealClock\(\)' \
  "${guard_paths[@]}" -g '*.go' -g '!**/*_test.go' >"${guard_hits}" || status=$?
# rg returns 1 for no matches; actual scan failures must never become a pass.
(( status <= 1 )) || exit "${status}"

declare -A expected=() seen=()
while IFS='|' read -r file statement classification reason count extra; do
  [[ -z "${file}" || "${file}" == \#* ]] && continue
  case "${classification}" in
    business_time_boundary|infrastructure_time|external_protocol_time|db_audit_timestamp) ;;
    *) echo "invalid clock classification: ${file}: ${classification}" >&2; exit 1 ;;
  esac
  if [[ -z "${statement}" || -z "${reason//[[:space:]]/}" || ! "${count}" =~ ^[1-9][0-9]*$ || -n "${extra}" ]]; then
    echo "malformed clock allowlist entry: ${file}" >&2; exit 1
  fi
  [[ -f "${guard_root}/${file}" ]] || { echo "stale clock allowlist path: ${file}" >&2; exit 1; }
  key="${file}|${statement}"
  [[ ! -v expected["${key}"] ]] || { echo "duplicate clock allowlist entry: ${file}" >&2; exit 1; }
  expected["${key}"]="${count}"
  seen["${key}"]=0
done <"${guard_allowlist}"

failures=0
while IFS= read -r hit; do
  rel="${hit#${guard_root}/}"
  file="${rel%%:*}"; rest="${rel#*:}"; line="${rest%%:*}"; statement="${rest#*:}"
  statement="${statement#"${statement%%[![:space:]]*}"}"
  statement="${statement%"${statement##*[![:space:]]}"}"
  [[ "${statement}" == //* || "${statement}" == \** ]] && continue
  key="${file}|${statement}"
  if [[ -v expected["${key}"] ]]; then
    seen["${key}"]=$(( ${seen["${key}"]} + 1 ))
  else
    printf '%s:%s: unclassified business-time usage: %s\n' "${file}" "${line}" "${statement}" >&2
    failures=$((failures + 1))
  fi
done <"${guard_hits}"

for key in "${!expected[@]}"; do
  if [[ "${seen["${key}"]}" != "${expected["${key}"]}" ]]; then
    printf 'clock allowlist occurrence changed: %s (expected %s, found %s)\n' "${key}" "${expected["${key}"]}" "${seen["${key}"]}" >&2
    failures=$((failures + 1))
  fi
done
if (( failures > 0 )); then
  echo "business-time guardrail failed; inject the owning clock or review the exact classified boundary" >&2
  exit 1
fi
echo "business-time guardrail passed"
