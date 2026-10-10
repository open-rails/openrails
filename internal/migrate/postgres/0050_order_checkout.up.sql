-- parent: 49 sha256:0bf9655698fb1d24c0c0074d5d943c4c90f78c4cfecca2d59b35a389f388432d
-- Repair: none-needed New nullable columns; no existing order has a hosted checkout.
-- A merchant order's hosted checkout: the URL's secret is never stored, only
-- its sha256, which the checkout host looks the order up by.
ALTER TABLE billing.orders
    ADD COLUMN checkout_secret_hash bytea,
    ADD COLUMN checkout_expires_at timestamp with time zone,
    ADD COLUMN checkout_success_url text,
    ADD COLUMN checkout_cancel_url text,
    ADD COLUMN checkout_saved_payment_methods boolean,
    ADD CONSTRAINT orders_checkout_check CHECK (
        (checkout_secret_hash IS NULL) = (checkout_expires_at IS NULL)
        AND (checkout_secret_hash IS NULL) = (checkout_success_url IS NULL)
        AND (checkout_secret_hash IS NULL) = (checkout_saved_payment_methods IS NULL)
        AND (checkout_cancel_url IS NULL OR checkout_secret_hash IS NOT NULL)
        AND (checkout_secret_hash IS NULL OR (origin = 'merchant' AND octet_length(checkout_secret_hash) = 32 AND checkout_expires_at <= expires_at))),
    ADD CONSTRAINT orders_checkout_success_url_check CHECK (checkout_success_url <> ''),
    ADD CONSTRAINT orders_checkout_cancel_url_check CHECK (checkout_cancel_url <> '');

CREATE UNIQUE INDEX orders_checkout_secret_hash_key ON billing.orders USING btree (checkout_secret_hash) WHERE checkout_secret_hash IS NOT NULL;

COMMENT ON COLUMN billing.orders.checkout_secret_hash IS 'sha256 of the hosted checkout URL''s secret; the secret is never stored. Set once, on a merchant order.';
COMMENT ON COLUMN billing.orders.checkout_expires_at IS 'When the checkout URL stops working; no later than the order''s own expiry.';
