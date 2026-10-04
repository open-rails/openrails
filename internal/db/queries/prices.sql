-- billing.prices.

-- name: CreatePrice :execrows
INSERT INTO billing.prices (
    id, merchant_id, product_id, archived, amount, currency,
    access_duration_hours, auto_renew, trial_unit_amount, trial_duration_hours, key, created_at, updated_at
) SELECT
    sqlc.arg(id),
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(product_id),
    sqlc.arg(archived)::boolean,
    sqlc.arg(amount), sqlc.arg(currency),
    sqlc.narg(access_duration_hours), sqlc.arg(auto_renew)::boolean, sqlc.narg(trial_unit_amount), sqlc.narg(trial_duration_hours),
    sqlc.arg(key)::text,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
FROM billing.products catalog_product
WHERE catalog_product.merchant_id=sqlc.arg(merchant_id)::uuid
  AND catalog_product.id=sqlc.arg(product_id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid);

-- name: GetPriceByID :one
SELECT price.* FROM billing.prices price
WHERE price.merchant_id=sqlc.arg(merchant_id)::uuid AND price.id=sqlc.arg(id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (
    SELECT 1 FROM billing.products catalog_product
    WHERE catalog_product.merchant_id=price.merchant_id
      AND catalog_product.id=price.product_id
      AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid));

-- name: ListPricesByIDs :many
SELECT * FROM billing.prices WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=prices.merchant_id AND catalog_product.id=prices.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND prices.merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListPricesWithProductByIDs :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.id = price.product_id
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=price.merchant_id AND catalog_product.id=price.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND price.merchant_id = sqlc.arg(merchant_id)::uuid AND prod.merchant_id = sqlc.arg(merchant_id)::uuid AND price.id = ANY(sqlc.arg(ids)::uuid[]);

-- All prices for a product, archived included — the catalog converge needs
-- archived rows to reconcile legacy_import prices instead of re-creating them
-- (would violate unique_prices_product_amount_cycle).
-- name: ListPricesByProduct :many
SELECT * FROM billing.prices price
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=price.merchant_id AND catalog_product.id=price.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND price.merchant_id = sqlc.arg(merchant_id)::uuid AND price.product_id = $1;

-- name: ListCurrentPricesByProducts :many
SELECT price.* FROM billing.prices price
JOIN billing.products prod ON prod.merchant_id = price.merchant_id AND prod.id = price.product_id
WHERE price.merchant_id = sqlc.arg(merchant_id)::uuid AND price.product_id = ANY(sqlc.arg(product_ids)::uuid[])
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR prod.catalog_id = sqlc.narg(catalog_id)::uuid)
  AND NOT price.archived
ORDER BY price.amount, price.id;

-- name: ListActivePricesByProductOrdered :many
SELECT * FROM billing.prices price
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=price.merchant_id AND catalog_product.id=price.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND price.merchant_id = sqlc.arg(merchant_id)::uuid AND price.product_id = $1 AND NOT price.archived
ORDER BY price.amount ASC;

-- name: ListAllActivePricesWithProduct :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.id = price.product_id
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=price.merchant_id AND catalog_product.id=price.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND price.merchant_id = sqlc.arg(merchant_id)::uuid AND prod.merchant_id = sqlc.arg(merchant_id)::uuid AND NOT price.archived
ORDER BY price.amount ASC;

-- name: ListAllPricesWithProduct :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.id = price.product_id

WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=price.merchant_id AND catalog_product.id=price.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND price.merchant_id = sqlc.arg(merchant_id)::uuid AND prod.merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY price.amount ASC;

-- name: ListPricesFiltered :many
-- One keyset page, newest first: rows after (after_at, after_id).
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.merchant_id = price.merchant_id AND prod.id = price.product_id
WHERE price.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR prod.catalog_id = sqlc.narg(catalog_id)::uuid)
  AND (sqlc.narg(archived)::boolean IS NULL OR price.archived = sqlc.narg(archived)::boolean)
  AND (sqlc.narg(currency)::text IS NULL OR price.currency = sqlc.narg(currency)::text)
  AND (sqlc.narg(product_id)::uuid IS NULL OR price.product_id = sqlc.narg(product_id)::uuid)
  AND (sqlc.narg(auto_renew)::boolean IS NULL OR price.auto_renew = sqlc.narg(auto_renew)::boolean)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (price.created_at, price.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY price.created_at DESC, price.id DESC
LIMIT sqlc.arg(fetch_limit)::int;

-- name: GetPriceByNMIPlan :one
SELECT price.* FROM billing.prices price
JOIN billing.price_psp_bindings binding ON binding.merchant_id = price.merchant_id AND binding.price_id = price.id
JOIN billing.psps psp ON psp.merchant_id = binding.merchant_id AND psp.id = binding.psp_id
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=price.merchant_id AND catalog_product.id=price.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND binding.merchant_id = sqlc.arg(merchant_id)::uuid AND binding.psp_id = sqlc.arg(psp_id)::uuid
  AND psp.rail = sqlc.arg(rail)::text AND binding.plan_id = sqlc.arg(plan_id)::text;

-- name: GetPriceWithProductByCCBillPriceID :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.id = price.product_id AND prod.merchant_id = price.merchant_id
JOIN billing.price_psp_bindings binding ON binding.merchant_id = price.merchant_id AND binding.price_id = price.id
JOIN billing.psps psp ON psp.merchant_id = binding.merchant_id AND psp.id = binding.psp_id
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=price.merchant_id AND catalog_product.id=price.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND binding.merchant_id = sqlc.arg(merchant_id)::uuid AND binding.psp_id = sqlc.arg(psp_id)::uuid
  AND psp.rail = 'ccbill'
  AND ((sqlc.arg(object_kind)::text = 'flex' AND binding.flex_id = sqlc.arg(ccbill_price_id)::text) OR (sqlc.arg(object_kind)::text = 'recurring_billing_option' AND binding.recurring_billing_option_id = sqlc.arg(ccbill_price_id)::text));

-- name: GetPriceWithProductByStripePriceID :one
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.id = price.product_id AND prod.merchant_id = price.merchant_id
JOIN billing.price_psp_bindings binding ON binding.merchant_id = price.merchant_id AND binding.price_id = price.id
JOIN billing.psps psp ON psp.merchant_id = binding.merchant_id AND psp.id = binding.psp_id
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=price.merchant_id AND catalog_product.id=price.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND binding.merchant_id = sqlc.arg(merchant_id)::uuid AND binding.psp_id = sqlc.arg(psp_id)::uuid
  AND psp.rail = 'stripe' AND binding.price_ref = sqlc.arg(stripe_price_id)::text;

-- #662: a price's money/identity columns (product_id, amount, currency,
-- access_duration_hours, auto_renew, trial_*) are IMMUTABLE — a reprice creates
-- a new row and archives the old. Only the two mutable fields are settable, and
-- each has its own narrow query so the immutable columns cannot be SET at the DB
-- layer at all (not merely by caller convention). A change to any immutable
-- column is, by construction, a different price with a different deterministic id.

-- name: UpdatePriceStatus :execrows
UPDATE billing.prices AS price SET
    archived=sqlc.arg(archived)::boolean, updated_at=now()
WHERE price.merchant_id=sqlc.arg(merchant_id)::uuid AND price.id=sqlc.arg(id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (
    SELECT 1 FROM billing.products catalog_product
    WHERE catalog_product.merchant_id=price.merchant_id
      AND catalog_product.id=price.product_id
      AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid));

-- name: UpdatePriceKey :execrows
UPDATE billing.prices AS price SET
    key=sqlc.arg(key)::text, updated_at=now()
WHERE price.merchant_id=sqlc.arg(merchant_id)::uuid AND price.id=sqlc.arg(id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (
    SELECT 1 FROM billing.products catalog_product
    WHERE catalog_product.merchant_id=price.merchant_id
      AND catalog_product.id=price.product_id
      AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid));

-- name: GetCurrentPriceByKey :one
SELECT * FROM billing.prices
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=prices.merchant_id AND catalog_product.id=prices.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(key)::text AND NOT archived;

-- All rows (archived + current) ever pointed at by this key — the version chain.
-- name: ListPriceChainByKey :many
SELECT * FROM billing.prices
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=prices.merchant_id AND catalog_product.id=prices.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(key)::text
ORDER BY created_at ASC;

-- The archived members of a key's chain — #773's "all prior versions of key K".
-- name: ListPriorVersionsByKey :many
SELECT * FROM billing.prices
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM billing.products catalog_product WHERE catalog_product.merchant_id=prices.merchant_id AND catalog_product.id=prices.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(key)::text AND archived
ORDER BY created_at ASC;
