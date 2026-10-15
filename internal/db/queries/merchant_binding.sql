-- name: GetMerchantTenantBinding :one
SELECT id, slug, status, permission_group_id, tenant_id, deleted_at, retired_at
FROM billing.merchants WHERE id = sqlc.arg(id)::uuid;

-- name: LockMerchantTenantBinding :one
SELECT id, slug, status, permission_group_id, tenant_id, deleted_at, retired_at
FROM billing.merchants WHERE id = sqlc.arg(id)::uuid FOR UPDATE;

-- name: BindMerchantTenant :execrows
UPDATE billing.merchants
SET permission_group_id = id::text, tenant_id = sqlc.arg(tenant_id)::uuid, updated_at = now()
WHERE id = sqlc.arg(id)::uuid AND status = 'active' AND deleted_at IS NULL AND retired_at IS NULL
  AND (permission_group_id IS NULL OR permission_group_id = id::text)
  AND (tenant_id IS NULL OR tenant_id = sqlc.arg(tenant_id)::uuid);
