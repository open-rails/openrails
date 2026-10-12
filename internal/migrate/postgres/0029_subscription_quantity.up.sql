-- Repair: none-needed Every existing subscription bills one unit.
-- Every writer supplies it: the default only fills the existing rows.
ALTER TABLE billing.subscriptions ADD COLUMN quantity integer NOT NULL DEFAULT 1;
ALTER TABLE billing.subscriptions ALTER COLUMN quantity DROP DEFAULT;
ALTER TABLE billing.subscriptions ADD CONSTRAINT subscriptions_quantity_check CHECK (quantity >= 1);
ALTER TABLE billing.subscriptions ADD CONSTRAINT subscriptions_quantity_engine_check
    CHECK (quantity = 1 OR (collection_policy = 'engine' AND rail IN ('nmi', 'stripe')));
COMMENT ON COLUMN billing.subscriptions.quantity IS 'Seats: renewals bill the unit price times this. Above 1 only on an engine-owned NMI or Stripe subscription; provider-owned and Solana subscriptions bill one unit.';
