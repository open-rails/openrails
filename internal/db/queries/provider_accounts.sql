-- openrails.provider_accounts: merchant-scoped provider account registry (#518).

-- name: UpsertProviderAccount :one
INSERT INTO openrails.provider_accounts (
    merchant_id, provider_type, environment, account_id, display_name,
    vault_secret_ref, role, status, evidence, last_verified_at
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    lower(sqlc.arg(provider_type)::text),
    COALESCE(sqlc.narg(environment)::text, 'live'),
    sqlc.arg(account_id)::text,
    sqlc.narg(display_name),
    sqlc.narg(vault_secret_ref),
    COALESCE(
        sqlc.narg(role)::text,
        CASE
            WHEN EXISTS (
                SELECT 1
                FROM openrails.provider_accounts existing
                WHERE existing.merchant_id = sqlc.arg(merchant_id)::uuid
                  AND existing.provider_type = lower(sqlc.arg(provider_type)::text)
                  AND existing.environment = COALESCE(sqlc.narg(environment)::text, 'live')
                  AND existing.role = 'primary'
                  AND existing.status = 'enabled'
            ) THEN 'secondary'
            ELSE 'primary'
        END
    ),
    COALESCE(sqlc.narg(status)::text, 'enabled'),
    sqlc.narg(evidence),
    COALESCE(sqlc.narg(last_verified_at)::timestamptz, now())
)
ON CONFLICT (merchant_id, provider_type, environment, account_id) DO UPDATE SET
    display_name = COALESCE(EXCLUDED.display_name, openrails.provider_accounts.display_name),
    vault_secret_ref = COALESCE(EXCLUDED.vault_secret_ref, openrails.provider_accounts.vault_secret_ref),
    evidence = COALESCE(EXCLUDED.evidence, openrails.provider_accounts.evidence),
    last_verified_at = EXCLUDED.last_verified_at,
    updated_at = now()
RETURNING *;

-- name: GetProviderAccount :one
SELECT * FROM openrails.provider_accounts
WHERE id = $1;

-- name: GetProviderAccountByIdentity :one
SELECT * FROM openrails.provider_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND provider_type = lower(sqlc.arg(provider_type)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: DemoteOtherPrimaryProviderAccounts :exec
UPDATE openrails.provider_accounts
SET role = 'legacy',
    replaced_at = COALESCE(replaced_at, now()),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND provider_type = lower(sqlc.arg(provider_type)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND id <> sqlc.arg(id)::uuid
  AND role = 'primary'
  AND status = 'enabled';

-- name: PromoteProviderAccountToPrimary :one
UPDATE openrails.provider_accounts
SET role = 'primary',
    status = 'enabled',
    replaced_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id)::uuid
  AND merchant_id = sqlc.arg(merchant_id)::uuid
  AND provider_type = lower(sqlc.arg(provider_type)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
RETURNING *;

-- name: ListProviderAccountsForMerchant :many
SELECT * FROM openrails.provider_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(provider_type)::text IS NULL OR provider_type = lower(sqlc.narg(provider_type)::text))
ORDER BY provider_type, environment, role, created_at, id;
