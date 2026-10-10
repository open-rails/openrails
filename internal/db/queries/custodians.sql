-- billing.custodians: merchant-scoped custodian identities (or#880). A row is
-- one merchant-owned account with a third-party card custodian; its
-- configuration is the custodian document.

-- name: RegisterCustodian :one
-- Records a custodian document's identity. An identity another merchant owns
-- answers no row.
INSERT INTO billing.custodians (merchant_id, key, kind, environment, account_id)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(key)::text, lower(sqlc.arg(kind)::text), sqlc.arg(environment)::text, sqlc.arg(account_id)::text)
ON CONFLICT (kind, environment, account_id) DO UPDATE SET key = billing.custodians.key
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
