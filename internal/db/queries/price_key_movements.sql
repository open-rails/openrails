-- billing.price_key_movements (#774): pointer-movement history log.

-- name: InsertPriceKeyMovement :execrows
INSERT INTO billing.price_key_movements (
    merchant_id, key, price_id, effective_at, archived
) SELECT
    sqlc.arg(merchant_id)::uuid, sqlc.arg(key)::text, sqlc.arg(price_id)::uuid,
    COALESCE(NULLIF(sqlc.arg(effective_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), clock_timestamp()),
    (owned_price.archived OR catalog_product.archived)
FROM billing.prices owned_price JOIN billing.products catalog_product
  ON catalog_product.merchant_id=owned_price.merchant_id AND catalog_product.id=owned_price.product_id
WHERE owned_price.merchant_id=sqlc.arg(merchant_id)::uuid AND owned_price.id=sqlc.arg(price_id)::uuid;

-- name: ListPriceKeyMovements :many
SELECT movement.* FROM billing.price_key_movements movement
JOIN billing.prices price ON price.merchant_id = movement.merchant_id AND price.id = movement.price_id
WHERE movement.merchant_id = sqlc.arg(merchant_id)::uuid AND price.product_id = sqlc.arg(product_id)::uuid AND movement.key = sqlc.arg(key)::text
 AND (sqlc.narg(after_at)::timestamptz IS NULL OR (movement.effective_at, movement.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY movement.effective_at DESC, movement.id DESC
LIMIT sqlc.arg(fetch_limit)::int;

-- The price row that was current for `key` as of `as_of` — "what did key K
-- sell on date D".
