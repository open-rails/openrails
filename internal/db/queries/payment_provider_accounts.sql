-- openrails.payment_provider_accounts: merchant-scoped provider account registry (#518).

-- name: UpsertProviderAccount :one
INSERT INTO openrails.payment_provider_accounts (
    merchant_id, rail, environment, account_id, display_name,
    archived, evidence, last_verified_at
) VALUES (
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
    display_name = COALESCE(EXCLUDED.display_name, openrails.payment_provider_accounts.display_name),
    archived = EXCLUDED.archived,
    replaced_at = CASE
        WHEN EXCLUDED.archived THEN COALESCE(openrails.payment_provider_accounts.replaced_at, now())
        ELSE NULL
    END,
    evidence = COALESCE(EXCLUDED.evidence, openrails.payment_provider_accounts.evidence),
    last_verified_at = EXCLUDED.last_verified_at,
    updated_at = now()
WHERE openrails.payment_provider_accounts.merchant_id = EXCLUDED.merchant_id
RETURNING *;

-- name: GetProviderAccount :one
SELECT * FROM openrails.payment_provider_accounts
WHERE id = $1;

-- name: GetProviderAccountByIdentity :one
SELECT * FROM openrails.payment_provider_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: GetProviderAccountByRailIdentity :one
SELECT * FROM openrails.payment_provider_accounts
WHERE rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: ListProviderAccountsForMerchant :many
SELECT * FROM openrails.payment_provider_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(rail)::text IS NULL OR rail = lower(sqlc.narg(rail)::text))
ORDER BY rail, environment, archived, created_at, id;

-- name: GetActiveProviderAccountForNewWork :one
-- The newest non-archived account on a rail+environment. Existing provider-bound
-- work must use its recorded provider_account_id instead of this selector.
SELECT * FROM openrails.payment_provider_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND archived = false
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: CountActiveProviderAccountsForNewWork :one
SELECT count(*)::bigint FROM openrails.payment_provider_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND archived = false;

-- name: CountProviderAccountsForRailEnvironment :one
SELECT count(*)::bigint FROM openrails.payment_provider_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live');
