-- parent: 11 sha256:741ba093ab3b140e35e5ded12b9b27e49dec1d5692c684201433013aa170848c
-- #1091: a subscription awaiting a new card is live. It holds its product and
-- tier-group slot, keeps its method in use and blocks tier-group changes,
-- like past_due.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

DROP INDEX openrails.uq_subscriptions_customer_product_lifecycle;
CREATE UNIQUE INDEX uq_subscriptions_customer_product_lifecycle ON openrails.subscriptions USING btree (merchant_id, customer_id, product_id)
    WHERE status IN ('active', 'pending', 'past_due', 'awaiting_method') AND deleted_at IS NULL;

DROP INDEX openrails.uq_subscriptions_customer_tier_group_active;
CREATE UNIQUE INDEX uq_subscriptions_customer_tier_group_active ON openrails.subscriptions USING btree (merchant_id, customer_id, tier_group)
    WHERE status IN ('active', 'pending', 'past_due', 'awaiting_method', 'unverified') AND tier_group IS NOT NULL AND deleted_at IS NULL;

DROP INDEX openrails.ix_subscriptions_renewal_by_payment_method;
CREATE INDEX ix_subscriptions_renewal_by_payment_method ON openrails.subscriptions USING btree (payment_method_id, current_period_ends_at)
    WHERE deleted_at IS NULL AND payment_method_id IS NOT NULL AND status IN ('active', 'past_due', 'awaiting_method');

CREATE OR REPLACE FUNCTION openrails.products_guard_tier_group() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.tier_group IS DISTINCT FROM OLD.tier_group AND EXISTS (
        SELECT 1 FROM openrails.subscriptions s
        WHERE s.merchant_id = OLD.merchant_id AND s.product_id = OLD.id
          AND s.deleted_at IS NULL
          AND s.status IN ('active', 'pending', 'past_due', 'awaiting_method', 'unverified')
          AND (s.scheduled_price_id IS NOT NULL OR EXISTS (
                SELECT 1 FROM openrails.rail_intents i
                WHERE i.merchant_id = s.merchant_id AND i.subscription_id = s.id
                  AND i.intent_type IN ('nmi_upgrade', 'stripe_tier_change', 'initial_membership')
                  AND i.status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable')))
    ) THEN
        RAISE EXCEPTION 'product tier group cannot change while a live subscription has a plan change in flight'
            USING ERRCODE = '23514', CONSTRAINT = 'products_live_subscription_tier_group';
    END IF;
    RETURN NEW;
END;
$$;
