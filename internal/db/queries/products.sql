-- billing.products.

-- name: CreateProduct :execrows
INSERT INTO billing.products (
    id, tenant_id, slug, display_name, description, entitlements_spec,
    credits_spec, tier_group, tier_rank, status, created_at, updated_at
) VALUES (
    $1,
    COALESCE(NULLIF(sqlc.arg(tenant_id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid),
             '00000000-0000-0000-0000-000000000001'::uuid),
    $2, $3, sqlc.narg(description), sqlc.narg(entitlements_spec),
    sqlc.narg(credits_spec), sqlc.narg(tier_group),
    COALESCE(NULLIF(sqlc.arg(tier_rank)::int, 0), 0),
    COALESCE(NULLIF(sqlc.arg(status)::text, ''), 'active'),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetProductByID :one
SELECT * FROM billing.products WHERE id = $1;

-- name: GetProductBySlug :one
SELECT * FROM billing.products WHERE slug = $1;

-- name: ListProductsByIDs :many
SELECT * FROM billing.products WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListActiveProducts :many
SELECT * FROM billing.products WHERE status = 'active';

-- name: ListAllProducts :many
SELECT * FROM billing.products;

-- name: CountActiveProducts :one
SELECT count(*) FROM billing.products WHERE status = 'active';

-- name: ListActiveProductsPaged :many
SELECT * FROM billing.products
WHERE status = 'active'
ORDER BY created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: CountAllProducts :one
SELECT count(*) FROM billing.products;

-- name: ListAllProductsPaged :many
SELECT * FROM billing.products
ORDER BY created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: UpdateProduct :execrows
UPDATE billing.products SET
    slug = $2,
    display_name = $3,
    description = sqlc.narg(description),
    entitlements_spec = sqlc.narg(entitlements_spec),
    credits_spec = sqlc.narg(credits_spec),
    tier_group = sqlc.narg(tier_group),
    tier_rank = $4,
    status = $5,
    updated_at = sqlc.arg(updated_at)
WHERE id = $1;

-- name: DeleteProduct :execrows
DELETE FROM billing.products WHERE id = $1;
