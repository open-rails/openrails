-- parent: 24 sha256:991d4ede1222d8df27c0b57cb056adf11b31f57725e2873f4d3beb08491df8b1
-- Repair: none-needed The marker is cleared on every row that is not canceled
-- before its check; a stored pair of a live subscription and a stop-pending one
-- for the same customer and product is a customer billed twice, and the
-- indexes refuse to start on it rather than hide it.

-- A canceled subscription whose provider schedule may still bill keeps the
-- customer's product and tier-group slot until the stop is verified, on every
-- rail. Unverified subscriptions hold the product slot as they already hold
-- the tier-group slot.
UPDATE billing.subscriptions SET deletion_scheduled_at = NULL
WHERE status <> 'canceled' AND deletion_scheduled_at IS NOT NULL;
ALTER TABLE billing.subscriptions ADD CONSTRAINT subscriptions_stop_pending_canceled_check
    CHECK (status = 'canceled' OR deletion_scheduled_at IS NULL);
COMMENT ON COLUMN billing.subscriptions.deletion_scheduled_at IS 'Set while the provider schedule of a canceled subscription may still bill: from the local cancel until its stop intent verifies the schedule gone. Holds the customer''s product and tier-group slot. On NMI it is also when the deferred delete runs.';

DROP INDEX billing.subscriptions_customer_id_product_id_key;
CREATE UNIQUE INDEX subscriptions_customer_id_product_id_key ON billing.subscriptions USING btree (merchant_id, customer_id, product_id)
    WHERE deleted_at IS NULL AND (status IN ('active', 'pending', 'past_due', 'awaiting_method', 'unverified') OR deletion_scheduled_at IS NOT NULL);
DROP INDEX billing.subscriptions_customer_id_tier_group_key;
CREATE UNIQUE INDEX subscriptions_customer_id_tier_group_key ON billing.subscriptions USING btree (merchant_id, customer_id, tier_group)
    WHERE tier_group IS NOT NULL AND deleted_at IS NULL AND (status IN ('active', 'pending', 'past_due', 'awaiting_method', 'unverified') OR deletion_scheduled_at IS NOT NULL);
