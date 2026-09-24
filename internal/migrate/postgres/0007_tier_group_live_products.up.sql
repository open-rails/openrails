-- parent: 6 sha256:c0bef9e0b3e2d935d328b09020333086fb91dcf4e310157fa8ecb86dc0c1e722
-- A product's tier group can be assigned or changed while it has live
-- subscriptions (#1076): the change propagates to their denormalized
-- tier_group in the same statement, so the one-live-membership-per-group
-- index judges it (a customer holding two products joined into one group is
-- refused). Only a live subscription with a plan change in flight blocks it.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE OR REPLACE FUNCTION openrails.products_guard_tier_group() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.tier_group IS DISTINCT FROM OLD.tier_group AND EXISTS (
        SELECT 1 FROM openrails.subscriptions s
        WHERE s.merchant_id = OLD.merchant_id AND s.product_id = OLD.id
          AND s.deleted_at IS NULL
          AND s.status IN ('active', 'pending', 'past_due', 'unknown')
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

CREATE FUNCTION openrails.products_propagate_tier_group() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.tier_group IS DISTINCT FROM OLD.tier_group THEN
        UPDATE openrails.subscriptions SET tier_group = NEW.tier_group
        WHERE merchant_id = NEW.merchant_id AND product_id = NEW.id AND deleted_at IS NULL;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER trg_products_propagate_tier_group AFTER UPDATE OF tier_group ON openrails.products
    FOR EACH ROW EXECUTE FUNCTION openrails.products_propagate_tier_group();
