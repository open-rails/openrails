-- openrails.custom_credit_types (#475): per-tenant consume-only credit units.

-- name: DefineCustomCreditType :one
INSERT INTO openrails.custom_credit_types (
    id, tenant_id, name, decimals, active
) VALUES (
    uuidv7(), sqlc.arg(tenant_id)::uuid, sqlc.arg(name)::text, sqlc.arg(decimals)::int, true
)
ON CONFLICT (tenant_id, name) DO UPDATE
    SET decimals = EXCLUDED.decimals, active = true, updated_at = now()
RETURNING *;

-- name: ListCustomCreditTypes :many
SELECT * FROM openrails.custom_credit_types
WHERE tenant_id = sqlc.arg(tenant_id)::uuid
ORDER BY name;

-- name: GetCustomCreditType :one
SELECT * FROM openrails.custom_credit_types
WHERE tenant_id = sqlc.arg(tenant_id)::uuid AND name = sqlc.arg(name)::text;

-- name: SetCustomCreditTypeActive :one
UPDATE openrails.custom_credit_types
SET active = sqlc.arg(active)::boolean, updated_at = now()
WHERE tenant_id = sqlc.arg(tenant_id)::uuid AND name = sqlc.arg(name)::text
RETURNING *;
