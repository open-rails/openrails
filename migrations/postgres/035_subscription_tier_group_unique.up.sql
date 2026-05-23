-- Enforce one active/pending subscription per (user, tier group) on upgraded DBs.
-- Migration 002 introduced this invariant after the historical migration chain
-- was consolidated, but existing deployments had already recorded migration 2.
-- Re-issue the same idempotent invariant under a new forward migration number.

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

ALTER TABLE billing.subscriptions
    ADD COLUMN IF NOT EXISTS tier_group VARCHAR(100);

UPDATE billing.subscriptions AS sub
SET tier_group = prod.tier_group
FROM billing.products AS prod
WHERE prod.id = sub.product_id
  AND sub.tier_group IS DISTINCT FROM prod.tier_group;

CREATE OR REPLACE FUNCTION billing.subscriptions_set_tier_group()
RETURNS TRIGGER AS $$
BEGIN
    SELECT prod.tier_group INTO NEW.tier_group
    FROM billing.products AS prod
    WHERE prod.id = NEW.product_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_subscriptions_set_tier_group ON billing.subscriptions;
CREATE TRIGGER trg_subscriptions_set_tier_group
    BEFORE INSERT OR UPDATE OF product_id
    ON billing.subscriptions
    FOR EACH ROW
    EXECUTE FUNCTION billing.subscriptions_set_tier_group();

DO $$
DECLARE
    dup_count integer;
BEGIN
    SELECT count(*) INTO dup_count FROM (
        SELECT 1
        FROM billing.subscriptions
        WHERE status IN ('active', 'pending')
          AND tier_group IS NOT NULL
        GROUP BY user_id, tier_group
        HAVING count(*) > 1
    ) AS dups;

    IF dup_count > 0 THEN
        RAISE EXCEPTION
            'cannot enforce unique (user_id, tier_group): % active/pending duplicate tier-group group(s) exist; dedup them before applying this migration',
            dup_count;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriptions_user_tier_group_active
    ON billing.subscriptions (user_id, tier_group)
    WHERE status IN ('active', 'pending') AND tier_group IS NOT NULL;

COMMENT ON COLUMN billing.subscriptions.tier_group IS
    'Denormalized from billing.products.tier_group (kept in sync by trigger trg_subscriptions_set_tier_group). Backs uq_subscriptions_user_tier_group_active, which enforces one active/pending subscription per (user, tier group).';
