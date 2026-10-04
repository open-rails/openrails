-- billing.custodians: merchant-scoped custodian registry (or#880). A row is
-- one merchant-owned account with a third-party card custodian. Referenced by
-- psps.custodian_id — one custodian can back many PSPs.

-- name: UpsertCustodian :one
INSERT INTO billing.custodians (
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
    credential_versions = billing.custodians.credential_versions || COALESCE((
        SELECT jsonb_object_agg(incoming.key, greatest(
            incoming.value::bigint,
            (billing.custodians.credential_versions ->> incoming.key)::bigint
        ))
        FROM jsonb_each_text(EXCLUDED.credential_versions) AS incoming
    ), '{}'::jsonb),
    updated_at = now()
WHERE billing.custodians.merchant_id = EXCLUDED.merchant_id
RETURNING *;

-- name: GetCustodian :one
SELECT * FROM billing.custodians
WHERE custodians.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- name: GetCustodianByKey :one
SELECT * FROM billing.custodians
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND lower(key) = lower(sqlc.arg(key)::text)
LIMIT 1;

-- name: GetCustodianByIdentity :one
SELECT * FROM billing.custodians
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND kind = lower(sqlc.arg(kind)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: ListCustodiansForMerchant :many
SELECT * FROM billing.custodians
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY kind, key, id;

-- CROSS-MERCHANT: a custodian's own instrument events carry its tenant id and
-- no merchant context. This resolves the CUSTODIAN, never "the" PSP: one
-- custodian may back several.
-- name: ResolveCustodianOwnerByIdentity :one
SELECT id, merchant_id, key, kind, environment, account_id
FROM billing.custodians
WHERE kind = lower(sqlc.arg(kind)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text;
