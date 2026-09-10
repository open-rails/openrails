#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/bin" "$fixture/scripts"
cp "$root/scripts/e2e_dump_local.sh" "$fixture/scripts/e2e_dump_local.sh"
touch "$fixture/docker-compose.yaml"
stub="$fixture/bin/docker"

cat >"$stub" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail

sql="$(cat)"
[[ "$sql" == *"FROM openrails.checkout_sessions"* ]]
[[ "$sql" == *"FROM openrails.payment_methods"* ]]
[[ "$sql" == *"FROM openrails.subscriptions"* ]]
[[ "$sql" == *"FROM openrails.payments"* ]]
[[ "$sql" == *"SELECT id FROM openrails.customers WHERE subject = :'e2e_user_id'"* ]]
[[ "$sql" == *"metadata->>'e2e_run_id' = :'e2e_run_id'"* ]]
[[ "$sql" != *"FROM billing."* ]]
[[ "$sql" != *"SELECT id, user_id"* ]]
[[ "$sql" != *"vault_id"* ]]

args="$(printf '%s\n' "$@")"
grep -Fxq -- "e2e_run_id=$EXPECTED_RUN_ID" <<<"$args"
grep -Fxq -- "e2e_user_id=$EXPECTED_USER_ID" <<<"$args"
STUB
chmod +x "$stub"

PATH="$fixture/bin:$PATH" EXPECTED_RUN_ID="run-'quoted" EXPECTED_USER_ID="" \
  E2E_RUN_ID="run-'quoted" E2E_USER_ID="" bash "$fixture/scripts/e2e_dump_local.sh" >/dev/null

PATH="$fixture/bin:$PATH" EXPECTED_RUN_ID="" EXPECTED_USER_ID="00000000-0000-0000-0000-000000000001" \
  E2E_RUN_ID="" E2E_USER_ID="00000000-0000-0000-0000-000000000001" \
  bash "$fixture/scripts/e2e_dump_local.sh" >/dev/null

set +e
output="$(PATH="$fixture/bin:$PATH" E2E_RUN_ID="" E2E_USER_ID="" bash "$fixture/scripts/e2e_dump_local.sh" 2>&1)"
status=$?
set -e
[[ "$status" -ne 0 ]]
grep -q "Provide E2E_RUN_ID and/or E2E_USER_ID" <<<"$output"

echo "e2e dump regression tests passed"
