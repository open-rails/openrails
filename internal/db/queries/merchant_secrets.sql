-- DB-backed merchant secret store (the Vault store keeps the same addressing).

-- name: GetMerchantSecret :one
SELECT name, value, version FROM openrails.merchant_secrets
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND name = sqlc.arg(name)::text;

-- Re-putting the same value keeps its version.
-- name: PutMerchantSecret :one
INSERT INTO openrails.merchant_secrets (merchant_id, name, value, version)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(name)::text, sqlc.arg(value)::text, 1)
ON CONFLICT (merchant_id, name) DO UPDATE
    SET value = EXCLUDED.value,
        version = CASE WHEN openrails.merchant_secrets.value = EXCLUDED.value
                       THEN openrails.merchant_secrets.version
                       ELSE openrails.merchant_secrets.version + 1 END,
        updated_at = current_timestamp
RETURNING name, value, version;

-- name: StageMerchantSecret :exec
INSERT INTO openrails.merchant_secrets (merchant_id, name, value, version)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(name)::text, sqlc.arg(value)::text, 1)
ON CONFLICT (merchant_id, name) DO NOTHING;

-- name: DeleteMerchantSecret :exec
DELETE FROM openrails.merchant_secrets
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND name = sqlc.arg(name)::text;

-- name: DeleteAllMerchantSecrets :exec
DELETE FROM openrails.merchant_secrets WHERE merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: ListMerchantSecretNames :many
SELECT name FROM openrails.merchant_secrets
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY name;
