-- Repair: none-needed No customer order has a checkout yet, so the reshaped check refuses no stored row; the new column starts empty.
-- A customer's own order takes its checkout too (POST /v1/me/orders with a
-- checkout): embedded, returning nowhere (the frame tells its page), lasting
-- as long as the order. Asking again replaces the secret on the order.
ALTER TABLE billing.orders
    DROP CONSTRAINT orders_checkout_check,
    ADD CONSTRAINT orders_checkout_check CHECK (
        (checkout_secret_hash IS NULL) = (checkout_expires_at IS NULL)
        AND (checkout_secret_hash IS NULL) = (checkout_saved_payment_methods IS NULL)
        AND (checkout_success_url IS NULL OR checkout_secret_hash IS NOT NULL)
        AND (checkout_cancel_url IS NULL OR checkout_secret_hash IS NOT NULL)
        AND (checkout_secret_hash IS NULL OR (octet_length(checkout_secret_hash) = 32 AND checkout_expires_at <= expires_at
            AND (origin = 'merchant' AND checkout_success_url IS NOT NULL
                 OR origin = 'customer' AND checkout_success_url IS NULL AND checkout_cancel_url IS NULL AND checkout_ui_mode = 'embedded'))));

COMMENT ON COLUMN billing.orders.checkout_secret_hash IS 'sha256 of the hosted checkout URL''s secret; the secret is never stored. Set once on a merchant order; on a customer''s own order, again each time the customer asks for its checkout, which ends the last.';
COMMENT ON COLUMN billing.orders.checkout_success_url IS 'Where a merchant order''s checkout returns once paid. NULL on a customer''s own embedded checkout, which returns nowhere: the frame tells its page.';

-- A customer asking again for the same lines gets its open order back: one
-- open order per customer and lines (prices, quantities, amounts).
ALTER TABLE billing.orders
    ADD COLUMN lines_digest bytea,
    ADD CONSTRAINT orders_lines_digest_check CHECK (lines_digest IS NULL OR (origin = 'customer' AND octet_length(lines_digest) = 32));

CREATE UNIQUE INDEX orders_open_lines_key ON billing.orders USING btree (merchant_id, customer_id, lines_digest)
    WHERE status = 'open' AND lines_digest IS NOT NULL;

COMMENT ON COLUMN billing.orders.lines_digest IS 'sha256 of a customer order''s lines (prices, quantities, amounts), which POST /v1/me/orders finds its open order by. NULL on a cart''s order and every other origin.';
