#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ALLOWLIST="${ROOT}/docs/business-time-allowlist.txt"

if [[ ! -f "${ALLOWLIST}" ]]; then
  echo "missing ${ALLOWLIST}" >&2
  exit 1
fi

tmp="$(mktemp)"
trap 'rm -f "${tmp}"' EXIT

rg -n --no-heading 'time\.Now\(|\bNOW\(\)|CURRENT_TIMESTAMP|clockwork\.NewRealClock\(\)' \
  "${ROOT}/internal/modules" \
  "${ROOT}/internal/river" \
  "${ROOT}/internal/db/repo" \
  "${ROOT}/internal/http/handlers" \
  "${ROOT}/pkg/service" \
  -g '!**/*_test.go' >"${tmp}" || true

failures=0
while IFS= read -r hit; do
  rel="${hit#${ROOT}/}"
  file="${rel%%:*}"
  rest="${rel#*:}"
  line="${rest%%:*}"
  text="${rest#*:}"
  trimmed="${text#"${text%%[![:space:]]*}"}"

  # Comments are allowed to mention time.Now/NOW while explaining clock behavior.
  if [[ "${trimmed}" == //* || "${trimmed}" == \** || "${trimmed}" == \#* ]]; then
    continue
  fi

  allowed=false
  while IFS='|' read -r allowed_file fragment classification reason; do
    [[ -z "${allowed_file}" || "${allowed_file}" == \#* ]] && continue
    if [[ "${file}" == "${allowed_file}" && "${text}" == *"${fragment}"* ]]; then
      allowed=true
      break
    fi
  done <"${ALLOWLIST}"

  if [[ "${allowed}" == false ]]; then
    printf '%s:%s: unclassified business-time usage: %s\n' "${file}" "${line}" "${trimmed}" >&2
    failures=$((failures + 1))
  fi
done <"${tmp}"

if (( failures > 0 )); then
  echo "business-time guardrail failed; either inject the runtime clock or add a classified allowlist entry" >&2
  exit 1
fi

echo "business-time guardrail passed"
