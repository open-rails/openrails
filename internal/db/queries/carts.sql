-- The customer's cart: lines priced live by the order engine.

-- name: GetCart :one
SELECT lines, last_changed_at FROM billing.carts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid;

-- LockCart takes the cart's row lock for one change, creating an empty cart
-- when there is none: the conflict update locks the row it finds, and a row
-- removed meanwhile is inserted afresh.
-- name: LockCart :one
INSERT INTO billing.carts (merchant_id, customer_id, lines, last_changed_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, '[]'::jsonb, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, customer_id) DO UPDATE SET lines = billing.carts.lines
RETURNING lines, last_changed_at;

-- name: SetCartLines :exec
UPDATE billing.carts SET lines = sqlc.arg(lines)::jsonb, last_changed_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid;

-- Retention: a cart goes 30 days after its last change. Bounded: row_limit
-- caps one statement and the cleanup worker loops; SKIP LOCKED leaves a cart
-- being changed to its change.
-- name: DeleteExpiredCarts :execrows
DELETE FROM billing.carts c
USING (
    SELECT merchant_id, customer_id FROM billing.carts
    WHERE last_changed_at < sqlc.arg(before)::timestamptz
    ORDER BY last_changed_at
    LIMIT sqlc.arg(row_limit)::int
    FOR UPDATE SKIP LOCKED
) expired
WHERE c.merchant_id = expired.merchant_id AND c.customer_id = expired.customer_id
  AND c.last_changed_at < sqlc.arg(before)::timestamptz;
