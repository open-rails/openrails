-- openrails.custom_credit_types (#475): per-tenant consume-only credit units.
-- Rows are written by the catalog sidecar push (#706, auto-defined from
-- catalog_credit_balances.unit); this read backs ResolveUnit.

-- name: GetCustomCreditType :one
SELECT * FROM openrails.custom_credit_types
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND name = sqlc.arg(name)::text;

-- name: GetCustomCreditTypeByID :one
SELECT * FROM openrails.custom_credit_types
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid;

-- name: EnsureCustomCreditType :one
INSERT INTO openrails.custom_credit_types(id,merchant_id,name,decimals,active)
VALUES(uuidv7(),sqlc.arg(merchant_id)::uuid,sqlc.arg(name)::text,0,true)
ON CONFLICT(merchant_id,name) DO UPDATE SET active=true,updated_at=now()
RETURNING *;
