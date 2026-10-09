-- parent: 38 sha256:690fe1558d03aa33f991a192897987e715730705ea89550a2a8c9b94bbdcc351
-- Repair: none-needed No price was sold per seat, so every quantity becomes NULL; nothing billed seats before.
-- A recurring price is sold per seat only when it declares its bounds; every
-- quantity below is NULL unless it is.
ALTER TABLE billing.prices ADD COLUMN quantity jsonb;
ALTER TABLE billing.prices ADD CONSTRAINT prices_quantity_terms CHECK (quantity IS NULL OR (
    jsonb_typeof(quantity) = 'object' AND billing_interval_hours IS NOT NULL AND customer_amount IS NULL
    AND quantity ? 'min' AND quantity ? 'max'
    AND COALESCE((quantity->>'min')::int >= 1, false)
    AND COALESCE((quantity->>'max')::int BETWEEN (quantity->>'min')::int AND 10000, false)
));
ALTER TABLE billing.prices DROP CONSTRAINT prices_product_amount_window_key;
ALTER TABLE billing.prices ADD CONSTRAINT prices_product_amount_window_key
    UNIQUE NULLS NOT DISTINCT (merchant_id, product_id, key, amount, currency, access_duration_hours, billing_interval_hours, trial_unit_amount, trial_duration_hours, customer_amount, quantity);
COMMENT ON COLUMN billing.prices.quantity IS 'Seat bounds {min, max} of a recurring price sold per seat; amount is per seat. NULL: the price has no quantity.';

ALTER TABLE billing.subscriptions DROP CONSTRAINT subscriptions_quantity_check;
ALTER TABLE billing.subscriptions DROP CONSTRAINT subscriptions_quantity_engine_check;
ALTER TABLE billing.subscriptions ALTER COLUMN quantity DROP NOT NULL;
UPDATE billing.subscriptions SET quantity = NULL;
-- The price's bounds are enforced by the writers; the database cannot see them.
ALTER TABLE billing.subscriptions ADD CONSTRAINT subscriptions_quantity_check CHECK (quantity IS NULL OR (
    quantity >= 1 AND collection_policy = 'engine' AND rail IN ('nmi', 'stripe')));
COMMENT ON COLUMN billing.subscriptions.quantity IS 'Seats of a per-seat price: renewals bill the unit price times this. NULL unless the price is sold per seat; only an engine-owned NMI or Stripe subscription has seats.';

ALTER TABLE billing.rebill_cycles ADD COLUMN quantity integer CHECK (quantity IS NULL OR quantity >= 1);
COMMENT ON COLUMN billing.rebill_cycles.quantity IS 'Seats the cycle bills; NULL unless the subscription is per seat.';

ALTER TABLE billing.payments ADD COLUMN quantity integer;
ALTER TABLE billing.payments ADD CONSTRAINT payments_quantity_check CHECK (quantity IS NULL OR (quantity >= 1 AND subscription_id IS NOT NULL));
COMMENT ON COLUMN billing.payments.quantity IS 'Seats a per-seat subscription payment billed: a period at that many seats, or the seats a mid-period increase added. NULL otherwise.';

ALTER TABLE billing.grants ADD COLUMN quantity integer CHECK (quantity IS NULL OR quantity >= 1);
COMMENT ON COLUMN billing.grants.quantity IS 'Seats an access grant gives: its per-seat subscription''s quantity; NULL otherwise.';
ALTER TABLE billing.product_access ADD COLUMN quantity integer CHECK (quantity IS NULL OR quantity >= 1);
COMMENT ON COLUMN billing.product_access.quantity IS 'Seats the window gives, from its grant; NULL unless per seat.';
ALTER TABLE billing.customer_entitlement_cache ADD COLUMN quantity integer CHECK (quantity IS NULL OR quantity >= 1);
COMMENT ON COLUMN billing.customer_entitlement_cache.quantity IS 'The most seats of the key a held per-seat product gives; NULL when none is per seat.';
