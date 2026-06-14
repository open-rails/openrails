-- openrails.custom_credit_types (#475): per-tenant consume-only credit units.

-- name: DefineCustomCreditType :one
INSERT INTO openrails.custom_credit_types (
    id, merchant_id, name, decimals, active
) VALUES (
    uuidv7(), sqlc.arg(merchant_id)::uuid, sqlc.arg(name)::text, sqlc.arg(decimals)::int, true
)
ON CONFLICT (merchant_id, name) DO UPDATE
    SET decimals = EXCLUDED.decimals, active = true, updated_at = now()
RETURNING *;

-- name: ListCustomCreditTypes :many
SELECT * FROM openrails.custom_credit_types
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY name;

-- name: GetCustomCreditType :one
SELECT * FROM openrails.custom_credit_types
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND name = sqlc.arg(name)::text;

-- name: SetCustomCreditTypeActive :one
UPDATE openrails.custom_credit_types
SET active = sqlc.arg(active)::boolean, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND name = sqlc.arg(name)::text
RETURNING *;
