-- A merchant order's Idempotency-Key is unique per merchant, whichever customer
-- the order is for: the same key with another customer is
-- idempotency_key_reused, never a second order. A key already reused for
-- another customer keeps its first order; each later order's key becomes
-- idempotency_key || ':' || id, so the unique index can be built and no
-- replay finds the later order by the shared key.
DROP INDEX billing.orders_idempotency_key_key;
UPDATE billing.orders o SET idempotency_key = o.idempotency_key || ':' || o.id
WHERE o.idempotency_key IS NOT NULL
  AND EXISTS (SELECT 1 FROM billing.orders e
              WHERE e.merchant_id = o.merchant_id AND e.idempotency_key = o.idempotency_key
                AND (e.created_at, e.id) < (o.created_at, o.id));
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

-- A finished Stripe resume only carried an instruction to Stripe, as a cancel
-- did: retention deletes it after the same period.
DROP INDEX billing.provider_intents_updated_at_idx;
CREATE INDEX provider_intents_updated_at_idx ON billing.provider_intents (merchant_id, updated_at)
    WHERE status IN ('succeeded', 'failed_terminal', 'superseded', 'expired') AND destructive_run_id IS NULL AND intent_type IN ('nmi_delete_subscription', 'stripe_cancel_subscription', 'stripe_resume_subscription', 'ccbill_cancel_subscription', 'nmi_payment_method_update', 'nmi_payment_source_update', 'nmi_card_vault', 'network_token', 'stripe_archive_price', 'stripe_archive_product', 'solana_sunset_plan', 'account_updater');
