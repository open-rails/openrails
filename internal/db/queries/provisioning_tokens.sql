-- name: ListProvisioningTokens :many
SELECT * FROM billing.provisioning_tokens
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY created_at, id;

-- name: CreateProvisioningToken :one
INSERT INTO billing.provisioning_tokens (merchant_id, name, declared, token_sha256)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(name)::text, false, sqlc.arg(token_sha256)::bytea)
RETURNING *;

-- name: DeleteProvisioningToken :execrows
DELETE FROM billing.provisioning_tokens
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: DeleteDeclaredProvisioningTokens :exec
-- The declared token but the one the declaration names now (none when it
-- names none).
DELETE FROM billing.provisioning_tokens
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND declared
  AND token_sha256 IS DISTINCT FROM sqlc.narg(keep_sha256)::bytea;

-- name: DeclareProvisioningToken :exec
INSERT INTO billing.provisioning_tokens (merchant_id, name, declared, token_sha256)
VALUES (sqlc.arg(merchant_id)::uuid, 'declared', true, sqlc.arg(token_sha256)::bytea)
ON CONFLICT DO NOTHING;

-- name: ResolveProvisioningToken :one
-- The token a directory presents, whichever merchant it belongs to.
SELECT merchant_id, id FROM billing.provisioning_tokens
WHERE token_sha256 = sqlc.arg(token_sha256)::bytea;

-- name: TouchProvisioningToken :exec
-- At most one write a minute per token.
UPDATE billing.provisioning_tokens SET last_used_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');
