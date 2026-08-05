-- openrails.custodians: merchant-scoped custodian registry (or#880). A row is
-- one merchant-owned account with a third-party card custodian. Referenced by
-- psps.custodian_id — one custodian can back many PSPs.

-- name: UpsertCustodian :one
INSERT INTO openrails.custodians (
    merchant_id, key, kind, environment, account_id, settings, archived, credential_versions
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(key)::text,
    lower(sqlc.arg(kind)::text),
    COALESCE(sqlc.narg(environment)::text, 'live'),
    sqlc.arg(account_id)::text,
    COALESCE(sqlc.narg(settings), '{}'::jsonb),
    COALESCE(sqlc.narg(archived)::boolean, false),
    COALESCE(sqlc.narg(credential_versions), '{}'::jsonb)
)
ON CONFLICT (kind, environment, account_id) DO UPDATE SET
    key = EXCLUDED.key,
    settings = EXCLUDED.settings,
    archived = EXCLUDED.archived,
    -- or#812: a floor NEVER goes backwards. An upsert that carries no floors
    -- (the manifest plane, which seeds rather than rotates) leaves the stored
    -- ones alone rather than clearing a rotation another writer recorded.
    credential_versions = CASE
        WHEN EXCLUDED.credential_versions = '{}'::jsonb THEN openrails.custodians.credential_versions
        ELSE openrails.custodians.credential_versions || EXCLUDED.credential_versions
    END,
    updated_at = now()
WHERE openrails.custodians.merchant_id = EXCLUDED.merchant_id
RETURNING *;

-- name: GetCustodian :one
SELECT * FROM openrails.custodians
WHERE id = $1;

-- name: GetCustodianByKey :one
SELECT * FROM openrails.custodians
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND lower(key) = lower(sqlc.arg(key)::text)
LIMIT 1;

-- name: GetCustodianByIdentity :one
SELECT * FROM openrails.custodians
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND kind = lower(sqlc.arg(kind)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: ListCustodiansForMerchant :many
SELECT * FROM openrails.custodians
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY kind, key, id;

-- CROSS-MERCHANT: a custodian sends its own instrument events carrying ITS
-- tenant id and no merchant context — the same problem as an account-routed
-- rail webhook, and the same SECURITY DEFINER answer (migration 0053). It
-- resolves the CUSTODIAN, never "the" PSP: one custodian may back several.
-- name: ResolveCustodianOwnerByIdentity :one
SELECT id, merchant_id, key, kind, environment, account_id
FROM openrails.custodian_owner_by_identity(
    lower(sqlc.arg(kind)::text),
    COALESCE(sqlc.narg(environment)::text, 'live'),
    sqlc.arg(account_id)::text
);
