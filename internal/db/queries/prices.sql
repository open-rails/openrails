-- billing.prices.

-- name: CreatePrice :one
INSERT INTO billing.prices (
    id, merchant_id, product_id, archived, amount, currency,
    access_duration_hours, billing_interval_hours, trial_unit_amount, trial_duration_hours, customer_amount, quantity, key, provider_owned, sell_via, provider_psp_id, provider_snapshot, billing_interval_months, provider_access_default, created_at, updated_at
) SELECT
    sqlc.arg(id),
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(product_id),
    sqlc.arg(archived)::boolean,
    sqlc.arg(amount), sqlc.arg(currency),
    sqlc.narg(access_duration_hours), sqlc.narg(billing_interval_hours)::int, sqlc.narg(trial_unit_amount), sqlc.narg(trial_duration_hours),
    sqlc.narg(customer_amount)::jsonb,
    sqlc.narg(quantity)::jsonb,
    sqlc.arg(key)::text,
    sqlc.arg(provider_owned)::boolean,
    sqlc.narg(sell_via)::text[],
    sqlc.narg(provider_psp_id)::uuid, sqlc.narg(provider_snapshot)::jsonb, sqlc.narg(billing_interval_months)::int, sqlc.arg(provider_access_default)::boolean,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
FROM billing.products catalog_product
WHERE catalog_product.merchant_id=sqlc.arg(merchant_id)::uuid
  AND catalog_product.id=sqlc.arg(product_id)::uuid
  AND catalog_product.deleted_at IS NULL
RETURNING prices.*;

-- Reads leave deleted prices out: nothing ever referred to them.

-- name: GetPriceByID :one
SELECT price.* FROM billing.prices price
WHERE price.merchant_id=sqlc.arg(merchant_id)::uuid AND price.id=sqlc.arg(id)::uuid AND price.deleted_at IS NULL;

-- name: ListPricesByIDs :many
SELECT * FROM billing.prices WHERE prices.merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[]) AND deleted_at IS NULL;

-- name: ListPricesWithProductByIDs :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.id = price.product_id
WHERE price.merchant_id = sqlc.arg(merchant_id)::uuid AND prod.merchant_id = sqlc.arg(merchant_id)::uuid AND price.id = ANY(sqlc.arg(ids)::uuid[])
  AND price.deleted_at IS NULL
ORDER BY price.created_at DESC, price.id DESC;

-- All prices for a product, archived included: the catalog apply matches
-- archived rows rather than re-creating them (prices_product_amount_window_key).
-- name: ListPricesByProduct :many
SELECT * FROM billing.prices price
WHERE price.merchant_id = sqlc.arg(merchant_id)::uuid AND price.product_id = $1 AND price.deleted_at IS NULL;

-- name: ListCurrentPricesByProducts :many
SELECT price.* FROM billing.prices price
JOIN billing.products prod ON prod.merchant_id = price.merchant_id AND prod.id = price.product_id
WHERE price.merchant_id = sqlc.arg(merchant_id)::uuid AND price.product_id = ANY(sqlc.arg(product_ids)::uuid[])
  AND NOT price.archived
ORDER BY price.amount, price.id;

-- name: ListActivePricesByProductOrdered :many
SELECT * FROM billing.prices price
WHERE price.merchant_id = sqlc.arg(merchant_id)::uuid AND price.product_id = $1 AND NOT price.archived
ORDER BY price.amount ASC;

-- name: ListAllActivePricesWithProduct :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.id = price.product_id
WHERE price.merchant_id = sqlc.arg(merchant_id)::uuid AND prod.merchant_id = sqlc.arg(merchant_id)::uuid AND NOT price.archived
ORDER BY price.amount ASC;

-- name: ListAllPricesWithProduct :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.id = price.product_id
WHERE price.merchant_id = sqlc.arg(merchant_id)::uuid AND prod.merchant_id = sqlc.arg(merchant_id)::uuid AND price.deleted_at IS NULL
ORDER BY price.amount ASC;

-- name: ListPricesFiltered :many
-- One keyset page, newest first: rows after (after_at, after_id).
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.merchant_id = price.merchant_id AND prod.id = price.product_id
WHERE price.merchant_id = sqlc.arg(merchant_id)::uuid AND price.deleted_at IS NULL
  AND (sqlc.narg(archived)::boolean IS NULL OR price.archived = sqlc.narg(archived)::boolean)
  AND (sqlc.narg(currency)::text IS NULL OR price.currency = sqlc.narg(currency)::text)
  AND (sqlc.narg(product_id)::uuid IS NULL OR price.product_id = sqlc.narg(product_id)::uuid)
  AND (sqlc.narg(product_key)::text IS NULL OR prod.key = sqlc.narg(product_key)::text)
  AND (sqlc.narg(key)::text IS NULL OR price.key = sqlc.narg(key)::text)
  AND (sqlc.narg(recurring)::boolean IS NULL OR (price.billing_interval_hours IS NOT NULL OR price.billing_interval_months IS NOT NULL) = sqlc.narg(recurring)::boolean)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (price.created_at, price.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY price.created_at DESC, price.id DESC
LIMIT sqlc.arg(fetch_limit)::int;

-- name: GetPriceByNMIPlan :one
SELECT price.* FROM billing.prices price
JOIN billing.price_psp_bindings binding ON binding.merchant_id = price.merchant_id AND binding.price_id = price.id
JOIN billing.psps psp ON psp.merchant_id = binding.merchant_id AND psp.id = binding.psp_id
WHERE binding.merchant_id = sqlc.arg(merchant_id)::uuid AND binding.psp_id = sqlc.arg(psp_id)::uuid
  AND psp.rail = sqlc.arg(rail)::text AND binding.plan_id = sqlc.arg(plan_id)::text;

-- name: GetPriceWithProductByCCBillPriceID :many
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.id = price.product_id AND prod.merchant_id = price.merchant_id
JOIN billing.price_psp_bindings binding ON binding.merchant_id = price.merchant_id AND binding.price_id = price.id
JOIN billing.psps psp ON psp.merchant_id = binding.merchant_id AND psp.id = binding.psp_id
WHERE binding.merchant_id = sqlc.arg(merchant_id)::uuid AND binding.psp_id = sqlc.arg(psp_id)::uuid
  AND psp.rail = 'ccbill'
  AND ((sqlc.arg(object_kind)::text = 'flex' AND binding.flex_id = sqlc.arg(ccbill_price_id)::text) OR (sqlc.arg(object_kind)::text = 'recurring_billing_option' AND binding.recurring_billing_option_id = sqlc.arg(ccbill_price_id)::text));

-- name: GetPriceWithProductByStripePriceID :one
SELECT sqlc.embed(price), sqlc.embed(prod)
FROM billing.prices price
JOIN billing.products prod ON prod.id = price.product_id AND prod.merchant_id = price.merchant_id
JOIN billing.price_psp_bindings binding ON binding.merchant_id = price.merchant_id AND binding.price_id = price.id
JOIN billing.psps psp ON psp.merchant_id = binding.merchant_id AND psp.id = binding.psp_id
WHERE binding.merchant_id = sqlc.arg(merchant_id)::uuid AND binding.psp_id = sqlc.arg(psp_id)::uuid
  AND psp.rail = 'stripe' AND binding.price_ref = sqlc.arg(stripe_price_id)::text;

-- A price's money and identity columns are immutable: a reprice creates a new
-- row and archives the old. Only archived is settable, by its own narrow query.

-- name: UpdatePriceStatus :execrows
UPDATE billing.prices AS price SET
    archived=sqlc.arg(archived)::boolean, updated_at=now()
WHERE price.merchant_id=sqlc.arg(merchant_id)::uuid AND price.id=sqlc.arg(id)::uuid;

-- name: UpdatePriceSellVia :execrows
UPDATE billing.prices AS price SET
    sell_via=sqlc.narg(sell_via)::text[], updated_at=now()
WHERE price.merchant_id=sqlc.arg(merchant_id)::uuid AND price.id=sqlc.arg(id)::uuid AND NOT price.provider_owned;

-- name: GetCurrentPriceByKey :one
SELECT * FROM billing.prices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid AND key = sqlc.arg(key)::text AND NOT archived;

-- name: GetCurrentPriceByProductKey :one
SELECT price.* FROM billing.prices price
JOIN billing.products product ON product.merchant_id = price.merchant_id AND product.id = price.product_id
WHERE price.merchant_id = sqlc.arg(merchant_id)::uuid AND product.key = sqlc.arg(product_key)::text
  AND price.key = sqlc.arg(key)::text AND NOT price.archived;

-- All rows (archived + current) ever pointed at by this key — the version chain.
-- name: ListPriceChainByKey :many
SELECT * FROM billing.prices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid AND key = sqlc.arg(key)::text AND deleted_at IS NULL
ORDER BY created_at ASC;

-- The archived members of a key's chain: its prior versions.
-- name: ListPriorVersionsByKey :many
SELECT * FROM billing.prices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid AND key = sqlc.arg(key)::text AND archived AND deleted_at IS NULL
ORDER BY created_at ASC;

-- name: GetPriceByTerms :one
SELECT * FROM billing.prices
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND product_id=sqlc.arg(product_id)::uuid AND key=sqlc.arg(key)::text
  AND amount=sqlc.arg(amount)::bigint AND currency=sqlc.arg(currency)::text
  AND access_duration_hours IS NOT DISTINCT FROM sqlc.narg(access_duration_hours)::int
  AND billing_interval_hours IS NOT DISTINCT FROM sqlc.narg(billing_interval_hours)::int
  AND billing_interval_months IS NOT DISTINCT FROM sqlc.narg(billing_interval_months)::int
  AND trial_unit_amount IS NOT DISTINCT FROM sqlc.narg(trial_unit_amount)::bigint
  AND trial_duration_hours IS NOT DISTINCT FROM sqlc.narg(trial_duration_hours)::int
  AND customer_amount IS NOT DISTINCT FROM sqlc.narg(customer_amount)::jsonb
  AND quantity IS NOT DISTINCT FROM sqlc.narg(quantity)::jsonb;

-- Private identity resolution deliberately includes retained tombstones.
-- Public catalog reads resolve first, then use the ordinary visible-row reads.
-- name: LookupPriceStorageID :one
SELECT id FROM billing.prices WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND public_id = sqlc.arg(public_id)::text
  AND sha256(public_id::bytea) = sha256(sqlc.arg(public_id)::text::bytea);

-- name: ListPriceReferences :many
SELECT id, public_id FROM billing.prices WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListPriceStorageIDs :many
-- Canonical IDs identify at most one row per merchant. Make the input-sized
-- result bound explicit, including empty/NULL batches, without a text-key cap.
SELECT id, public_id FROM billing.prices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND public_id = ANY(sqlc.arg(ids)::text[])
  AND sha256(public_id::bytea) IN (SELECT sha256(id::bytea) FROM unnest(sqlc.arg(ids)::text[]) AS id)
LIMIT COALESCE(cardinality(sqlc.arg(ids)::text[]), 0);

-- name: RevivePrice :execrows
-- Declaring a deleted price's terms again revives its row (the id is the
-- terms').
UPDATE billing.prices AS price SET archived = sqlc.arg(archived)::boolean, deleted_at = NULL, updated_at = now()
WHERE price.merchant_id = sqlc.arg(merchant_id)::uuid AND price.id = sqlc.arg(id)::uuid AND price.deleted_at IS NOT NULL;

-- name: ListProductPriceIDs :many
-- Every version of every key of a product, archived included.
SELECT id FROM billing.prices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid AND deleted_at IS NULL;

-- name: ListPriceKeyIDs :many
-- Every version of one price key, archived included.
SELECT id FROM billing.prices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid AND key = sqlc.arg(key)::text AND deleted_at IS NULL;

-- name: PricesHoldReferences :one
-- Whether a record refers to any of the prices: a purchase line,
-- a subscription, a purchase, a price migration, a scheduled change, a
-- provider write or a PSP link.
SELECT (
    EXISTS (SELECT 1 FROM billing.order_lines WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND price_id = ANY(sqlc.arg(price_ids)::uuid[]))
    OR EXISTS (SELECT 1 FROM billing.subscriptions WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND price_id = ANY(sqlc.arg(price_ids)::uuid[]))
    OR EXISTS (SELECT 1 FROM billing.price_migrations WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND from_price_id = ANY(sqlc.arg(price_ids)::uuid[]))
    OR EXISTS (SELECT 1 FROM billing.price_migrations WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND to_price_id = ANY(sqlc.arg(price_ids)::uuid[]))
    OR EXISTS (SELECT 1 FROM billing.scheduled_changes WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND price_id = ANY(sqlc.arg(price_ids)::uuid[]))
    OR EXISTS (SELECT 1 FROM billing.scheduled_changes WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND from_price_id = ANY(sqlc.arg(price_ids)::uuid[]))
    OR EXISTS (SELECT 1 FROM billing.provider_intents WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND price_id = ANY(sqlc.arg(price_ids)::uuid[]))
    OR EXISTS (SELECT 1 FROM billing.price_psp_bindings WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND price_id = ANY(sqlc.arg(price_ids)::uuid[]))
)::boolean AS referenced;

-- name: DeleteProductPrices :exec
-- A deleted product's prices go with it; deleted is always archived.
UPDATE billing.prices SET archived = true, deleted_at = now(), updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid AND deleted_at IS NULL;

-- name: DeletePriceKey :exec
-- Deletes every version of a price key nothing refers to.
UPDATE billing.prices SET archived = true, deleted_at = now(), updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid AND key = sqlc.arg(key)::text AND deleted_at IS NULL;
