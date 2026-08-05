-- openrails.products.

-- name: CreateProduct :execrows
INSERT INTO openrails.products (
    id, merchant_id, key, display_name, description, entitlements_spec,
    credits_spec, tier_group, tier_rank, archived, created_at, updated_at
) VALUES (
    $1,
    sqlc.arg(merchant_id)::uuid,
    $2, $3, sqlc.narg(description), sqlc.narg(entitlements_spec),
    sqlc.narg(credits_spec), sqlc.narg(tier_group),
    COALESCE(NULLIF(sqlc.arg(tier_rank)::int, 0), 0),
    sqlc.arg(archived)::boolean,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetProductByID :one
SELECT * FROM openrails.products WHERE id = $1;

-- name: GetProductByKey :one
SELECT * FROM openrails.products WHERE key = $1;

-- name: ListProductsByIDs :many
SELECT * FROM openrails.products WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListActiveProducts :many
SELECT * FROM openrails.products WHERE NOT archived;

-- name: ListAllProducts :many
SELECT * FROM openrails.products;

-- name: CountActiveProducts :one
SELECT count(*) FROM openrails.products WHERE NOT archived;

-- name: ListActiveProductsPaged :many
SELECT * FROM openrails.products
WHERE NOT archived
ORDER BY created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: CountAllProducts :one
SELECT count(*) FROM openrails.products;

-- name: ListAllProductsPaged :many
SELECT * FROM openrails.products
ORDER BY created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: UpdateProduct :execrows
UPDATE openrails.products SET
    key = $2,
    display_name = $3,
    description = sqlc.narg(description),
    entitlements_spec = sqlc.narg(entitlements_spec),
    credits_spec = sqlc.narg(credits_spec),
    tier_group = sqlc.narg(tier_group),
    tier_rank = $4,
    archived = $5,
    updated_at = sqlc.arg(updated_at)
WHERE id = $1;
