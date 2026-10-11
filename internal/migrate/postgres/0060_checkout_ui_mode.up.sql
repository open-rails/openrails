-- parent: 59 sha256:e01d09d0eea16f7bafb39049513fdf37e476891554200f60ec8e3cac9b021673
-- Repair: none-needed Every checkout so far is hosted, and the backfill says so.
-- Where an order's checkout page runs: hosted (the customer is sent to it) or
-- embedded (the merchant's registered origins frame it).
ALTER TABLE billing.orders ADD COLUMN checkout_ui_mode text;

UPDATE billing.orders SET checkout_ui_mode = 'hosted' WHERE checkout_secret_hash IS NOT NULL;

ALTER TABLE billing.orders
    ADD CONSTRAINT orders_checkout_ui_mode_check CHECK (
        checkout_ui_mode IN ('hosted', 'embedded') AND checkout_secret_hash IS NOT NULL
        OR checkout_ui_mode IS NULL AND checkout_secret_hash IS NULL);

-- The checkout host frames an embedded order's page by the order id in its
-- path, before any credential.
CREATE INDEX orders_checkout_embedded_idx ON billing.orders USING btree (id) WHERE checkout_ui_mode = 'embedded';

COMMENT ON COLUMN billing.orders.checkout_ui_mode IS 'Where the checkout page runs: hosted, or embedded in the merchant''s registered origins (settings checkout.embed_origins).';
