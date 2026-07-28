-- openrails.price_key_movements (#774): pointer-movement history log.

-- name: InsertPriceKeyMovement :execrows
INSERT INTO openrails.price_key_movements (
    merchant_id, key, price_id, effective_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(key)::text, sqlc.arg(price_id)::uuid,
    COALESCE(NULLIF(sqlc.arg(effective_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: ListPriceKeyMovements :many
SELECT * FROM openrails.price_key_movements
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(key)::text
ORDER BY effective_at DESC;

-- The price row that was current for `key` as of `as_of` — "what did key K
-- sell on date D".