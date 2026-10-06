-- billing.products.

-- name: CreateProduct :execrows
INSERT INTO billing.products (
    id, merchant_id, key, display_name, description, entitlements_spec,
    tier_group, tier_rank, archived, created_at, updated_at
) VALUES (
    $1,
    sqlc.arg(merchant_id)::uuid,
    $2, $3, sqlc.narg(description), sqlc.narg(entitlements_spec),
    NULLIF(sqlc.narg(tier_group)::text, ''),
    COALESCE(NULLIF(sqlc.arg(tier_rank)::int, 0), 0),
    sqlc.arg(archived)::boolean,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetProductByID :one
SELECT * FROM billing.products WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- name: GetProductByKey :one
SELECT * FROM billing.products WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND key = $1;

-- name: ListProductsByIDs :many
SELECT * FROM billing.products WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListActiveProducts :many
SELECT * FROM billing.products WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND NOT archived;

-- name: ListAllProducts :many
SELECT * FROM billing.products
WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid
;

-- name: ListProductsFiltered :many
-- One keyset page, newest first: rows after (after_at, after_id).
SELECT * FROM billing.products
WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(archived)::boolean IS NULL OR archived = sqlc.narg(archived)::boolean)
  AND (sqlc.arg(tier_group)::text = '' OR lower(btrim(tier_group)) = lower(btrim(sqlc.arg(tier_group)::text)))
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(fetch_limit)::int;

-- name: PatchProduct :one
-- Every field is chosen at the write point, never copied from a stale read.
UPDATE billing.products SET
    display_name = COALESCE(sqlc.narg(display_name)::text, display_name),
    description = CASE WHEN sqlc.arg(set_description)::boolean THEN NULLIF(sqlc.narg(description)::text, '') ELSE description END,
    entitlements_spec = CASE WHEN sqlc.arg(set_entitlements)::boolean THEN sqlc.narg(entitlements_spec)::jsonb ELSE entitlements_spec END,
    tier_group = CASE WHEN sqlc.arg(set_tier_group)::boolean THEN NULLIF(sqlc.narg(tier_group)::text, '') ELSE tier_group END,
    tier_rank = COALESCE(sqlc.narg(tier_rank)::int, tier_rank),
    archived = COALESCE(sqlc.narg(archived)::boolean, archived),
    updated_at = now()
WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
RETURNING *;
