-- openrails.rail_merchant_accounts: merchant-scoped provider account registry (#518).

-- name: UpsertRailMerchantAccount :one
INSERT INTO openrails.rail_merchant_accounts (
    id, merchant_id, rail, environment, account_id, display_name,
    archived, evidence, last_verified_at
) VALUES (
    sqlc.arg(id)::uuid,
    sqlc.arg(merchant_id)::uuid,
    lower(sqlc.arg(rail)::text),
    COALESCE(sqlc.narg(environment)::text, 'live'),
    sqlc.arg(account_id)::text,
    sqlc.narg(display_name),
    COALESCE(sqlc.narg(archived)::boolean, false),
    sqlc.narg(evidence),
    COALESCE(sqlc.narg(last_verified_at)::timestamptz, now())
)
ON CONFLICT (rail, environment, account_id) DO UPDATE SET
    display_name = COALESCE(EXCLUDED.display_name, openrails.rail_merchant_accounts.display_name),
    archived = EXCLUDED.archived,
    replaced_at = CASE
        WHEN EXCLUDED.archived THEN COALESCE(openrails.rail_merchant_accounts.replaced_at, now())
        ELSE NULL
    END,
    evidence = COALESCE(EXCLUDED.evidence, openrails.rail_merchant_accounts.evidence),
    last_verified_at = EXCLUDED.last_verified_at,
    updated_at = now()
WHERE openrails.rail_merchant_accounts.merchant_id = EXCLUDED.merchant_id
RETURNING *;

-- name: GetRailMerchantAccount :one
SELECT * FROM openrails.rail_merchant_accounts
WHERE id = $1;

-- name: GetRailMerchantAccountByIdentity :one
SELECT * FROM openrails.rail_merchant_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: GetRailMerchantAccountByRailIdentity :one
SELECT * FROM openrails.rail_merchant_accounts
WHERE rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: ListRailMerchantAccountsForMerchant :many
SELECT * FROM openrails.rail_merchant_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(rail)::text IS NULL OR rail = lower(sqlc.narg(rail)::text))
ORDER BY rail, environment, archived, created_at, id;

-- name: GetActiveRailMerchantAccountForNewWork :one
-- The newest non-archived account on a rail+environment. Existing provider-bound
-- work must use its recorded rail_merchant_account_id instead of this selector.
SELECT * FROM openrails.rail_merchant_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND archived = false
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: CountActiveRailMerchantAccountsForNewWork :one
SELECT count(*)::bigint FROM openrails.rail_merchant_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND archived = false;

-- name: CountRailMerchantAccountsForRailEnvironment :one
SELECT count(*)::bigint FROM openrails.rail_merchant_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live');
