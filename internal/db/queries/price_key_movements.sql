-- openrails.price_key_movements (#774): pointer-movement history log.

-- name: InsertPriceKeyMovement :execrows
INSERT INTO openrails.price_key_movements (
    merchant_id, key, price_id, effective_at, archived
) SELECT
    sqlc.arg(merchant_id)::uuid, sqlc.arg(key)::text, sqlc.arg(price_id)::uuid,
    COALESCE(NULLIF(sqlc.arg(effective_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), clock_timestamp()),
    (owned_price.archived OR catalog_product.archived)
FROM openrails.prices owned_price JOIN openrails.products catalog_product
  ON catalog_product.merchant_id=owned_price.merchant_id AND catalog_product.id=owned_price.product_id
WHERE owned_price.merchant_id=sqlc.arg(merchant_id)::uuid AND owned_price.id=sqlc.arg(price_id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid);

-- name: ListPriceKeyMovements :many
SELECT * FROM openrails.price_key_movements
WHERE (sqlc.narg(catalog_id)::uuid IS NULL OR EXISTS (
 SELECT 1 FROM openrails.prices owned_price JOIN openrails.products catalog_product
 ON catalog_product.merchant_id=owned_price.merchant_id AND catalog_product.id=owned_price.product_id
 WHERE owned_price.merchant_id=price_key_movements.merchant_id AND owned_price.id=price_key_movements.price_id
 AND catalog_product.catalog_id=sqlc.narg(catalog_id)::uuid))
 AND merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(key)::text
ORDER BY effective_at DESC, id DESC;

-- The price row that was current for `key` as of `as_of` — "what did key K
-- sell on date D".