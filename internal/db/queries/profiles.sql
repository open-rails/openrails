-- name: RegisterUnboundMerchant :one
-- Register a merchant (billing bucket) from config, idempotently (#480). The
-- merchant carries ONLY billing/rail state; NO auth. A re-register without a
-- display_name keeps any existing one (COALESCE), so config that omits it never
-- clears a name set elsewhere. A live group-bound merchant holding the name
-- returns no row: it is never adopted as a host-owned merchant.
INSERT INTO openrails.merchants (slug, status, display_name)
VALUES ($1, 'active', sqlc.narg(display_name))
ON CONFLICT (slug) WHERE deleted_at IS NULL DO UPDATE SET
    display_name = COALESCE(EXCLUDED.display_name, openrails.merchants.display_name),
    updated_at = now()
WHERE openrails.merchants.permission_group_id IS NULL
RETURNING id;

-- name: GetMerchantDirectoryByID :one
SELECT id,slug,status,permission_group_id,display_name,api_host
FROM openrails.merchants WHERE id=sqlc.arg(id)::uuid AND deleted_at IS NULL;

-- name: BindUnboundMerchantGroup :execrows
UPDATE openrails.merchants SET permission_group_id=sqlc.arg(group_id)::text,updated_at=now()
WHERE id=sqlc.arg(id)::uuid AND permission_group_id IS NULL AND deleted_at IS NULL;
