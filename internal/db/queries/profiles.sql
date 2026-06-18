-- Cross-schema read-only queries against authkit's profiles schema.
-- Compiled against internal/db/schema/profiles_shim.sql (see that file).

-- name: GetUserEmail :one
-- is_active is DERIVED as "not soft-deleted": authkit has no is_active
-- column (the previous bun query selected one and failed at runtime with
-- `column "is_active" does not exist`). deleted_at IS NULL is authkit's
-- active-user convention throughout its own queries.
SELECT
    COALESCE(username::text, '')::text AS username,
    COALESCE(email::text, '')::text AS email,
    email_verified,
    (deleted_at IS NULL)::boolean AS is_active
FROM profiles.users
WHERE id = $1;

-- name: GetUserIDByUsername :one
SELECT id
FROM profiles.users
WHERE username = $1 AND deleted_at IS NULL;

-- name: GetMerchantBySlug :one
SELECT id, slug, name, status
FROM openrails.merchants
WHERE slug = $1 AND deleted_at IS NULL;

-- name: RegisterMerchant :one
-- Register a merchant (billing bucket) from config, idempotently (#480). The
-- merchant carries ONLY billing/rail state; NO auth. Used by embedded boot and
-- standalone provisioning. Re-registering an existing slug refreshes the
-- billing-only fields and returns the canonical (self-owned) merchant id.
INSERT INTO openrails.merchants (slug, name, status, webhook_host, webhook_path)
VALUES ($1, $2, 'active', sqlc.narg('webhook_host'), sqlc.narg('webhook_path'))
ON CONFLICT (slug) DO UPDATE SET
    name = EXCLUDED.name,
    webhook_host = COALESCE(EXCLUDED.webhook_host, openrails.merchants.webhook_host),
    webhook_path = COALESCE(EXCLUDED.webhook_path, openrails.merchants.webhook_path),
    updated_at = now()
RETURNING id;
