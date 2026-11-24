-- A merchant order's Idempotency-Key is unique per merchant, whichever customer
-- the order is for: the same key with another customer is
-- idempotency_key_reused, never a second order.
DROP INDEX billing.orders_idempotency_key_key;
CREATE UNIQUE INDEX orders_idempotency_key_key ON billing.orders (merchant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- A subscription the provider confirmed ended, or whose first payment never
-- completed, keeps that cancel kind: stored as expired (a lapse), a reload
-- let a later payment or a resume revive it.
ALTER TABLE billing.subscriptions DROP CONSTRAINT subscriptions_cancel_type_check;
ALTER TABLE billing.subscriptions ADD CONSTRAINT subscriptions_cancel_type_check
    CHECK (cancel_type IN ('user', 'merchant', 'expired', 'chargeback', 'upgrade', 'provider', 'abandoned'));
ALTER TABLE billing.subscription_status_transitions DROP CONSTRAINT subscription_status_transitions_cancel_type_check;
ALTER TABLE billing.subscription_status_transitions ADD CONSTRAINT subscription_status_transitions_cancel_type_check
    CHECK (cancel_type IN ('user', 'merchant', 'expired', 'chargeback', 'upgrade', 'provider', 'abandoned'));
