-- openrails.products.

-- name: CreateProduct :execrows
INSERT INTO openrails.products (
    id, merchant_id, catalog_id, key, display_name, description, entitlements_spec,
    tier_group, tier_rank, archived, created_at, updated_at
) VALUES (
    $1,
    sqlc.arg(merchant_id)::uuid,
    sqlc.narg(catalog_id)::uuid,
    $2, $3, sqlc.narg(description), sqlc.narg(entitlements_spec),
    sqlc.narg(tier_group),
    COALESCE(NULLIF(sqlc.arg(tier_rank)::int, 0), 0),
    sqlc.arg(archived)::boolean,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetProductByID :one
SELECT * FROM openrails.products WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR products.catalog_id=sqlc.narg(catalog_id)::uuid) AND products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- name: GetProductByKey :one
SELECT * FROM openrails.products WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR products.catalog_id=sqlc.narg(catalog_id)::uuid) AND products.merchant_id = sqlc.arg(merchant_id)::uuid AND key = $1;

-- name: ListProductsByIDs :many
SELECT * FROM openrails.products WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR products.catalog_id=sqlc.narg(catalog_id)::uuid) AND products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListActiveProducts :many
SELECT * FROM openrails.products WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR products.catalog_id=sqlc.narg(catalog_id)::uuid) AND products.merchant_id = sqlc.arg(merchant_id)::uuid AND NOT archived;

-- name: ListAllProducts :many
SELECT * FROM openrails.products
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR products.catalog_id=sqlc.narg(catalog_id)::uuid) AND products.merchant_id = sqlc.arg(merchant_id)::uuid
;

-- name: CountProductsFiltered :one
SELECT count(*) FROM openrails.products
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR products.catalog_id=sqlc.narg(catalog_id)::uuid) AND products.merchant_id = sqlc.arg(merchant_id)::uuid AND (sqlc.narg(archived)::boolean IS NULL OR archived = sqlc.narg(archived)::boolean)
  AND (sqlc.arg(tier_group)::text = '' OR lower(btrim(tier_group)) = lower(btrim(sqlc.arg(tier_group)::text)));

-- name: ListProductsFiltered :many
SELECT * FROM openrails.products
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR products.catalog_id=sqlc.narg(catalog_id)::uuid) AND products.merchant_id = sqlc.arg(merchant_id)::uuid AND (sqlc.narg(archived)::boolean IS NULL OR archived = sqlc.narg(archived)::boolean)
  AND (sqlc.arg(tier_group)::text = '' OR lower(btrim(tier_group)) = lower(btrim(sqlc.arg(tier_group)::text)))
ORDER BY created_at DESC, id DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: PatchProduct :one
-- Every field is chosen at the write point, never copied from a stale read.
UPDATE openrails.products SET
    display_name = COALESCE(sqlc.narg(display_name)::text, display_name),
    description = CASE WHEN sqlc.arg(set_description)::boolean THEN NULLIF(sqlc.narg(description)::text, '') ELSE description END,
    entitlements_spec = CASE WHEN sqlc.arg(set_entitlements)::boolean THEN sqlc.narg(entitlements_spec)::jsonb ELSE entitlements_spec END,
    tier_group = CASE WHEN sqlc.arg(set_tier_group)::boolean THEN sqlc.narg(tier_group)::text ELSE tier_group END,
    tier_rank = COALESCE(sqlc.narg(tier_rank)::int, tier_rank),
    archived = COALESCE(sqlc.narg(archived)::boolean, archived),
    updated_at = now()
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR products.catalog_id=sqlc.narg(catalog_id)::uuid) AND products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
RETURNING *;
