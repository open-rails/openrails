-- Repair: none-needed A widened check refuses no stored row.
-- A customer's own order takes its checkout too (POST /v1/me/orders with a
-- checkout); asking again replaces the secret and expiry on the order.
ALTER TABLE billing.orders
    DROP CONSTRAINT orders_checkout_check,
    ADD CONSTRAINT orders_checkout_check CHECK (
        (checkout_secret_hash IS NULL) = (checkout_expires_at IS NULL)
        AND (checkout_secret_hash IS NULL) = (checkout_success_url IS NULL)
        AND (checkout_secret_hash IS NULL) = (checkout_saved_payment_methods IS NULL)
        AND (checkout_cancel_url IS NULL OR checkout_secret_hash IS NOT NULL)
        AND (checkout_secret_hash IS NULL OR (origin IN ('merchant', 'customer') AND octet_length(checkout_secret_hash) = 32 AND checkout_expires_at <= expires_at)));

COMMENT ON COLUMN billing.orders.checkout_secret_hash IS 'sha256 of the hosted checkout URL''s secret; the secret is never stored. Set once on a merchant order; on a customer''s own order, again each time the customer asks for its checkout, which ends the last.';
