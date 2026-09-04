-- openrails.prices.

-- name: CreatePrice :execrows
INSERT INTO openrails.prices (
    id, merchant_id, product_id, archived, amount, currency,
    access_duration_hours, auto_renew, trial_unit_amount, trial_duration_hours, psp_links, key, created_at, updated_at
) VALUES (
    $1,
    sqlc.arg(merchant_id)::uuid,
    $2,
    sqlc.arg(archived)::boolean,
    $3, $4,
    sqlc.narg(access_duration_hours), sqlc.arg(auto_renew)::boolean, sqlc.narg(trial_unit_amount), sqlc.narg(trial_duration_hours), sqlc.narg(psp_links),
    sqlc.arg(key)::text,
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

-- All prices for a product, archived included — the catalog converge needs
-- archived rows to reconcile legacy_import prices instead of re-creating them
-- (would violate unique_prices_product_amount_cycle).
-- name: ListPricesByProduct :many
SELECT * FROM openrails.prices price
WHERE price.product_id = $1;

-- name: ListActivePricesByProductOrdered :many
SELECT * FROM openrails.prices price
WHERE price.product_id = $1 AND NOT price.archived
ORDER BY price.amount ASC;

-- name: ListAllActivePricesWithProduct :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM openrails.prices price
JOIN openrails.products prod ON prod.id = price.product_id
WHERE NOT price.archived
ORDER BY price.amount ASC;

-- name: ListAllPricesWithProduct :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM openrails.prices price
JOIN openrails.products prod ON prod.id = price.product_id
ORDER BY price.amount ASC;

-- name: CountPricesFiltered :one
SELECT count(*) FROM openrails.prices price
WHERE (sqlc.narg(archived)::boolean IS NULL OR price.archived = sqlc.narg(archived)::boolean)
  AND (sqlc.narg(currency)::text IS NULL OR LOWER(price.currency) = LOWER(sqlc.narg(currency)::text))
  AND (sqlc.narg(product_id)::uuid IS NULL OR price.product_id = sqlc.narg(product_id)::uuid)
  AND (NOT sqlc.arg(only_recurring)::boolean OR price.auto_renew)
  AND (NOT sqlc.arg(only_one_time)::boolean OR NOT price.auto_renew);

-- name: ListPricesFiltered :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM openrails.prices price
JOIN openrails.products prod ON prod.id = price.product_id
WHERE (sqlc.narg(archived)::boolean IS NULL OR price.archived = sqlc.narg(archived)::boolean)
  AND (sqlc.narg(currency)::text IS NULL OR LOWER(price.currency) = LOWER(sqlc.narg(currency)::text))
  AND (sqlc.narg(product_id)::uuid IS NULL OR price.product_id = sqlc.narg(product_id)::uuid)
  AND (NOT sqlc.arg(only_recurring)::boolean OR price.auto_renew)
  AND (NOT sqlc.arg(only_one_time)::boolean OR NOT price.auto_renew)
ORDER BY price.created_at DESC, price.id DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: GetPriceByNMIPlan :one
-- psp_links entries key on the PSP key with the rail recorded inside; the
-- lookup name may be either the PSP key or the rail.
SELECT * FROM openrails.prices price
WHERE EXISTS (
    SELECT 1 FROM jsonb_each(price.psp_links) AS link(psp, cfg)
    WHERE cfg ->> 'plan_id' = sqlc.arg(plan_id)::text
      AND (link.psp = sqlc.arg(rail)::text OR cfg ->> 'rail' = sqlc.arg(rail)::text)
)
LIMIT 1;

-- name: GetPriceWithProductByCCBillPriceID :one
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM openrails.prices price
JOIN openrails.products prod ON prod.id = price.product_id
WHERE EXISTS (
    SELECT 1 FROM jsonb_each(price.psp_links) AS link(psp, cfg)
    WHERE cfg ->> 'rail' = 'ccbill'
      AND (cfg ->> 'flex_id' = sqlc.arg(ccbill_price_id)::text
           OR cfg ->> 'recurring_billing_option_id' = sqlc.arg(ccbill_price_id)::text)
)
LIMIT 1;

-- name: GetPriceWithProductByStripePriceID :one
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM openrails.prices price
JOIN openrails.products prod ON prod.id = price.product_id
WHERE EXISTS (
    SELECT 1 FROM jsonb_each(price.psp_links) AS link(psp, cfg)
    WHERE cfg ->> 'rail' = 'stripe' AND cfg ->> 'price_id' = sqlc.arg(stripe_price_id)::text
)
LIMIT 1;

-- #662: a price's money/identity columns (product_id, amount, currency,
-- access_duration_hours, auto_renew, trial_*) are IMMUTABLE — a reprice creates
-- a new row and archives the old. Only the two mutable fields are settable, and
-- each has its own narrow query so the immutable columns cannot be SET at the DB
-- layer at all (not merely by caller convention). A change to any immutable
-- column is, by construction, a different price with a different deterministic id.

-- name: UpdatePriceStatus :execrows
UPDATE openrails.prices SET
    archived = sqlc.arg(archived)::boolean,
    updated_at = now()
WHERE id = sqlc.arg(id);

-- name: UpdatePricePSPLinks :execrows
UPDATE openrails.prices SET
    psp_links = sqlc.narg(psp_links),
    updated_at = now()
WHERE id = sqlc.arg(id);

-- #774: key is a mutable LABEL (the movable pointer), not financial substance —
-- its own narrow query, same pattern as psp_links/archived above.
-- name: UpdatePriceKey :execrows
UPDATE openrails.prices SET
    key = sqlc.arg(key)::text,
    updated_at = now()
WHERE id = sqlc.arg(id);

-- name: GetCurrentPriceByKey :one
SELECT * FROM openrails.prices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(key)::text AND NOT archived;

-- All rows (archived + current) ever pointed at by this key — the version chain.
-- name: ListPriceChainByKey :many
SELECT * FROM openrails.prices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(key)::text
ORDER BY created_at ASC;

-- The archived members of a key's chain — #773's "all prior versions of key K".
-- name: ListPriorVersionsByKey :many
SELECT * FROM openrails.prices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(key)::text AND archived
ORDER BY created_at ASC;
