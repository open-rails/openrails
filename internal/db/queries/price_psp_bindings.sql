-- name: ListPricePSPBindings :many
-- provider_retired: the provider's catalog, as last observed, no longer sells
-- the bound plan or price (inactive, deleted, or its product is).
SELECT b.*, p.rail, p.key AS psp_key, (p.superseded_at IS NOT NULL)::boolean AS superseded,
    COALESCE(NOT o.active OR o.deleted_at IS NOT NULL OR COALESCE(NOT product.active OR product.deleted_at IS NOT NULL, false), false)::boolean AS provider_retired
FROM billing.price_psp_bindings b
JOIN billing.psps p ON p.id = b.psp_id AND p.merchant_id = b.merchant_id
LEFT JOIN billing.provider_catalog_objects o ON o.merchant_id = b.merchant_id AND o.psp_id = b.psp_id AND o.kind = 'price'
    AND o.ref = COALESCE(b.plan_id, b.price_ref)
LEFT JOIN billing.provider_catalog_objects product ON product.merchant_id = o.merchant_id AND product.psp_id = o.psp_id
    AND product.kind = 'product' AND product.ref = o.product_ref
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND (cardinality(sqlc.arg(price_ids)::uuid[]) = 0 OR b.price_id = ANY(sqlc.arg(price_ids)::uuid[]))
  AND (sqlc.narg(psp_id)::uuid IS NULL OR b.psp_id = sqlc.narg(psp_id)::uuid)
ORDER BY b.price_id, b.psp_id;

-- name: ResolvePriceBindingPSP :many
-- A label names the account it currently names: one whose label moved to
-- another account is superseded and bound only by its id.
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND rail = sqlc.arg(rail)::text
  AND ((sqlc.narg(psp_id)::uuid IS NOT NULL AND id = sqlc.narg(psp_id)::uuid)
       OR (sqlc.narg(psp_id)::uuid IS NULL AND key = sqlc.arg(psp_key)::text AND superseded_at IS NULL));

-- name: DeletePricePSPBindings :exec
DELETE FROM billing.price_psp_bindings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND price_id = sqlc.arg(price_id)::uuid;

-- name: InsertPricePSPBinding :exec
INSERT INTO billing.price_psp_bindings
(merchant_id, price_id, psp_id, plan_id, price_ref, recurring_billing_option_id, plan_pda, flex_id, configuration)
SELECT sqlc.arg(merchant_id)::uuid, sqlc.arg(price_id)::uuid, sqlc.arg(psp_id)::uuid,
    sqlc.narg(plan_id), sqlc.narg(price_ref), sqlc.narg(recurring_billing_option_id), sqlc.narg(plan_pda), sqlc.narg(flex_id), sqlc.arg(configuration)::jsonb
FROM billing.prices owned_price
WHERE owned_price.merchant_id=sqlc.arg(merchant_id)::uuid AND owned_price.id=sqlc.arg(price_id)::uuid;

-- name: LockPriceForBindingUpdate :one
SELECT id FROM billing.prices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(price_id)::uuid
FOR UPDATE;

-- name: ListProviderObjectHolders :many
-- The prices, deleted ones included, bound to one provider object of a PSP.
SELECT price.id, price.key, prod.key AS product_key, price.archived, (price.deleted_at IS NOT NULL)::boolean AS deleted
FROM billing.price_psp_bindings b
JOIN billing.prices price ON price.merchant_id = b.merchant_id AND price.id = b.price_id
JOIN billing.products prod ON prod.merchant_id = price.merchant_id AND prod.id = price.product_id
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.psp_id = sqlc.arg(psp_id)::uuid AND b.plan_pda IS NULL
  AND (b.plan_id = sqlc.narg(plan_id)::text OR b.price_ref = sqlc.narg(price_ref)::text
       OR b.recurring_billing_option_id = sqlc.narg(recurring_billing_option_id)::text)
ORDER BY price.created_at, price.id;
