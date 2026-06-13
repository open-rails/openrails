-- openrails.prices.

-- name: CreatePrice :execrows
INSERT INTO openrails.prices (
    id, tenant_id, product_id, status, amount, currency,
    billing_cycle_days, processors, created_at, updated_at
) VALUES (
    $1,
    sqlc.arg(tenant_id)::uuid,
    $2,
    COALESCE(NULLIF(sqlc.arg(status)::text, ''), 'active'),
    $3, $4,
    sqlc.narg(billing_cycle_days), sqlc.narg(processors),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetPriceByID :one
SELECT * FROM openrails.prices WHERE id = $1;

-- name: ListPricesByIDs :many
SELECT * FROM openrails.prices WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListPricesWithProductByIDs :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM openrails.prices price
JOIN openrails.products prod ON prod.id = price.product_id
WHERE price.id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListActivePricesByProduct :many
SELECT * FROM openrails.prices price
WHERE price.product_id = $1 AND price.status = 'active';

-- name: ListActivePricesByProductOrdered :many
SELECT * FROM openrails.prices price
WHERE price.product_id = $1 AND price.status = 'active'
ORDER BY price.amount ASC;

-- name: ListAllActivePricesWithProduct :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM openrails.prices price
JOIN openrails.products prod ON prod.id = price.product_id
WHERE price.status = 'active'
ORDER BY price.amount ASC;

-- name: ListAllPricesWithProduct :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM openrails.prices price
JOIN openrails.products prod ON prod.id = price.product_id
ORDER BY price.amount ASC;

-- name: CountPricesFiltered :one
SELECT count(*) FROM openrails.prices price
WHERE (NOT sqlc.arg(only_active)::boolean OR price.status = 'active')
  AND (NOT sqlc.arg(only_inactive)::boolean OR price.status <> 'active')
  AND (sqlc.narg(status)::text IS NULL OR price.status = sqlc.narg(status)::text)
  AND (sqlc.narg(currency)::text IS NULL OR LOWER(price.currency) = LOWER(sqlc.narg(currency)::text))
  AND (sqlc.narg(product_id)::uuid IS NULL OR price.product_id = sqlc.narg(product_id)::uuid)
  AND (NOT sqlc.arg(only_recurring)::boolean OR (price.billing_cycle_days IS NOT NULL AND price.billing_cycle_days > 0))
  AND (NOT sqlc.arg(only_one_time)::boolean OR (price.billing_cycle_days IS NULL OR price.billing_cycle_days = 0));

-- name: ListPricesFiltered :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM openrails.prices price
JOIN openrails.products prod ON prod.id = price.product_id
WHERE (NOT sqlc.arg(only_active)::boolean OR price.status = 'active')
  AND (NOT sqlc.arg(only_inactive)::boolean OR price.status <> 'active')
  AND (sqlc.narg(status)::text IS NULL OR price.status = sqlc.narg(status)::text)
  AND (sqlc.narg(currency)::text IS NULL OR LOWER(price.currency) = LOWER(sqlc.narg(currency)::text))
  AND (sqlc.narg(product_id)::uuid IS NULL OR price.product_id = sqlc.narg(product_id)::uuid)
  AND (NOT sqlc.arg(only_recurring)::boolean OR (price.billing_cycle_days IS NOT NULL AND price.billing_cycle_days > 0))
  AND (NOT sqlc.arg(only_one_time)::boolean OR (price.billing_cycle_days IS NULL OR price.billing_cycle_days = 0))
ORDER BY price.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: GetPriceByNMIPlan :one
-- include_nmi_fallback replicates the bun-era behavior: for non-"nmi"
-- providers the legacy "nmi" key is also consulted.
SELECT * FROM openrails.prices price
WHERE price.status <> 'draft'
  AND (price.processors -> sqlc.arg(provider)::text ->> 'plan_id' = sqlc.arg(plan_id)::text
       OR (sqlc.arg(include_nmi_fallback)::boolean
           AND price.processors -> 'nmi' ->> 'plan_id' = sqlc.arg(plan_id)::text))
LIMIT 1;

-- name: GetPriceWithProductByCCBillPriceID :one
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM openrails.prices price
JOIN openrails.products prod ON prod.id = price.product_id
WHERE price.processors -> 'ccbill' ->> 'flex_id' = sqlc.arg(flex_id)::text
  AND price.status <> 'draft'
LIMIT 1;

-- name: GetPriceWithProductByStripePriceID :one
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM openrails.prices price
JOIN openrails.products prod ON prod.id = price.product_id
WHERE price.processors -> 'stripe' ->> 'price_id' = sqlc.arg(stripe_price_id)::text
  AND price.status <> 'draft'
LIMIT 1;

-- name: UpdatePrice :execrows
UPDATE openrails.prices SET
    product_id = $2,
    status = $3,
    amount = $4,
    currency = $5,
    billing_cycle_days = sqlc.narg(billing_cycle_days),
    processors = sqlc.narg(processors),
    updated_at = sqlc.arg(updated_at)
WHERE id = $1;

-- name: DeletePrice :execrows
DELETE FROM openrails.prices WHERE id = $1;
