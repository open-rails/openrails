-- billing.products.

-- name: CreateProduct :execrows
INSERT INTO billing.products (
    id, merchant_id, key, display_name, description, credit_grant,
    tier_group, tier_rank, ownership, archived, created_at, updated_at
) VALUES (
    $1,
    sqlc.arg(merchant_id)::uuid,
    $2, $3, sqlc.narg(description), sqlc.narg(credit_grant),
    NULLIF(sqlc.narg(tier_group)::text, ''),
    COALESCE(NULLIF(sqlc.arg(tier_rank)::int, 0), 0),
    sqlc.narg(ownership)::text,
    sqlc.arg(archived)::boolean,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetProductByID :one
SELECT * FROM billing.products WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- name: GetProductByKey :one
SELECT * FROM billing.products WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND key = $1;

-- name: ListProductsByIDs :many
SELECT * FROM billing.products WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY created_at DESC, id DESC;

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
  AND (sqlc.narg(keys)::text[] IS NULL OR key = ANY (sqlc.narg(keys)::text[]))
  AND (sqlc.narg(entitlements)::text[] IS NULL OR id IN (
    SELECT pe.product_id FROM billing.product_entitlements pe
    WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid AND pe.entitlement = ANY (sqlc.narg(entitlements)::text[]) AND pe.removed_at IS NULL))
  -- For sale: some live price sells it. A product without one is granted only.
  AND (sqlc.narg(for_sale)::boolean IS NULL OR sqlc.narg(for_sale)::boolean = EXISTS (
    SELECT 1 FROM billing.prices pr
    WHERE pr.merchant_id = products.merchant_id AND pr.product_id = products.id AND NOT pr.archived))
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(fetch_limit)::int;

-- name: PatchProduct :one
-- Every field is chosen at the write point, never copied from a stale read.
UPDATE billing.products SET
    display_name = COALESCE(sqlc.narg(display_name)::text, display_name),
    description = CASE WHEN sqlc.arg(set_description)::boolean THEN NULLIF(sqlc.narg(description)::text, '') ELSE description END,
    credit_grant = CASE WHEN sqlc.arg(set_credit_grant)::boolean THEN sqlc.narg(credit_grant)::jsonb ELSE credit_grant END,
    tier_group = CASE WHEN sqlc.arg(set_tier_group)::boolean THEN NULLIF(sqlc.narg(tier_group)::text, '') ELSE tier_group END,
    tier_rank = COALESCE(sqlc.narg(tier_rank)::int, tier_rank),
    ownership = CASE WHEN sqlc.arg(set_ownership)::boolean THEN sqlc.narg(ownership)::text ELSE ownership END,
    archived = COALESCE(sqlc.narg(archived)::boolean, archived),
    revision = CASE WHEN sqlc.arg(keys_changed)::boolean THEN revision + 1 ELSE revision END,
    updated_at = now()
WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
RETURNING *;
