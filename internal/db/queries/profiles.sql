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
SELECT id, slug, status, display_name
FROM openrails.merchants
WHERE slug = $1 AND deleted_at IS NULL;

-- name: RegisterMerchant :one
-- Register a merchant (billing bucket) from config, idempotently (#480). The
-- merchant carries ONLY billing/rail state; NO auth. A re-register without a
-- display_name keeps any existing one (COALESCE), so config that omits it never
-- clears a name set elsewhere.
INSERT INTO openrails.merchants (slug, status, display_name)
VALUES ($1, 'active', sqlc.narg(display_name))
ON CONFLICT (slug) DO UPDATE SET
    display_name = COALESCE(EXCLUDED.display_name, openrails.merchants.display_name),
    updated_at = now()
RETURNING id;
