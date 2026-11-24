-- A merchant order's Idempotency-Key is unique per merchant, whichever customer
-- the order is for: the same key with another customer is
-- idempotency_key_reused, never a second order.
DROP INDEX billing.orders_idempotency_key_key;
CREATE UNIQUE INDEX orders_idempotency_key_key ON billing.orders (merchant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
