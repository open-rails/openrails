-- name: RegisterUnboundMerchant :one
-- Register a merchant (billing bucket) from config, idempotently (#480). The
-- merchant carries ONLY billing/rail state; NO auth. A live group-bound
-- merchant holding the name returns no row: it is never adopted as a
-- host-owned merchant.
INSERT INTO billing.merchants (slug, status)
VALUES ($1, 'active')
ON CONFLICT (slug) WHERE deleted_at IS NULL DO UPDATE SET updated_at = billing.merchants.updated_at
WHERE billing.merchants.permission_group_id IS NULL
RETURNING id;

-- name: GetMerchantDirectoryByID :one
SELECT id,slug,status,permission_group_id,api_host
FROM billing.merchants WHERE id=sqlc.arg(id)::uuid AND deleted_at IS NULL;

-- name: BindUnboundMerchantGroup :execrows
UPDATE billing.merchants SET permission_group_id=sqlc.arg(group_id)::text,updated_at=now()
WHERE id=sqlc.arg(id)::uuid AND permission_group_id IS NULL AND deleted_at IS NULL;
