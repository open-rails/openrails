-- name: ListPricePSPBindings :many
SELECT b.*, p.rail, COALESCE(p.key, p.id::text)::text AS psp_key
FROM openrails.price_psp_bindings b
JOIN openrails.psps p ON p.id = b.psp_id AND p.merchant_id = b.merchant_id
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM openrails.prices owned_price JOIN openrails.products catalog_product ON catalog_product.merchant_id=owned_price.merchant_id AND catalog_product.id=owned_price.product_id WHERE owned_price.merchant_id=b.merchant_id AND owned_price.id=b.price_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND b.merchant_id = sqlc.arg(merchant_id)::uuid AND (cardinality(sqlc.arg(price_ids)::uuid[]) = 0 OR b.price_id = ANY(sqlc.arg(price_ids)::uuid[]))
  AND (sqlc.narg(psp_id)::uuid IS NULL OR b.psp_id = sqlc.narg(psp_id)::uuid)
ORDER BY b.price_id, b.psp_id;

-- name: ResolvePriceBindingPSP :many
SELECT * FROM openrails.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND rail = sqlc.arg(rail)::text
  AND ((sqlc.narg(psp_id)::uuid IS NOT NULL AND id = sqlc.narg(psp_id)::uuid)
       OR (sqlc.narg(psp_id)::uuid IS NULL AND key = sqlc.arg(psp_key)::text));

-- name: DeletePricePSPBindings :exec
DELETE FROM openrails.price_psp_bindings
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM openrails.prices owned_price JOIN openrails.products catalog_product ON catalog_product.merchant_id=owned_price.merchant_id AND catalog_product.id=owned_price.product_id WHERE owned_price.merchant_id=price_psp_bindings.merchant_id AND owned_price.id=price_psp_bindings.price_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND merchant_id = sqlc.arg(merchant_id)::uuid AND price_id = sqlc.arg(price_id)::uuid;

-- name: InsertPricePSPBinding :exec
INSERT INTO openrails.price_psp_bindings
(merchant_id, price_id, psp_id, plan_id, price_ref, recurring_billing_option_id, plan_pda, flex_id, configuration)
SELECT sqlc.arg(merchant_id)::uuid, sqlc.arg(price_id)::uuid, sqlc.arg(psp_id)::uuid,
    sqlc.narg(plan_id), sqlc.narg(price_ref), sqlc.narg(recurring_billing_option_id), sqlc.narg(plan_pda), sqlc.narg(flex_id), sqlc.arg(configuration)::jsonb
FROM openrails.prices owned_price JOIN openrails.products catalog_product
  ON catalog_product.merchant_id=owned_price.merchant_id AND catalog_product.id=owned_price.product_id
WHERE owned_price.merchant_id=sqlc.arg(merchant_id)::uuid AND owned_price.id=sqlc.arg(price_id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid);

-- name: LockPriceForBindingUpdate :one
SELECT id FROM openrails.prices
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM openrails.products catalog_product WHERE catalog_product.merchant_id=prices.merchant_id AND catalog_product.id=prices.product_id AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid)) AND merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(price_id)::uuid
FOR UPDATE;
