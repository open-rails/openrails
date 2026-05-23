-- =============================================================================
-- 002 — enforce one active/pending subscription per (user, tier group)
--
-- Issue #213: the application-level same-tier-group guard
-- (CheckoutService.Checkout -> GetActiveOrPendingByUserIDAndTierGroup) only
-- reads the local DB, which is populated by webhooks. If a webhook is missed
-- the guard does not fire and a second parallel subscription slips through.
-- This migration adds a database-level invariant so duplicates are impossible
-- regardless of webhook delivery.
--
-- billing.subscriptions has no tier_group column today — tier_group lives on
-- billing.products and the runtime guard joins through product_id. A partial
-- UNIQUE index needs the value on the row, so we denormalize tier_group onto
-- subscriptions and keep it in sync with a trigger (no application code has to
-- remember to set it). The index then enforces:
--
--   UNIQUE (user_id, tier_group) WHERE status IN ('active','pending')
--
-- NOTE ON CONCURRENCY / TRANSACTIONS:
-- This project applies each migration file inside a single transaction
-- (see internal/migrate + open-rails/migratekit). CREATE INDEX CONCURRENTLY
-- cannot run in a transaction, so we use a plain CREATE UNIQUE INDEX. This is
-- the same pattern used by every other index in 001_schema.up.sql and is safe;
-- it briefly takes a lock on billing.subscriptions while building.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- 1. Denormalized tier_group column (NULL when the product has no tier group).
ALTER TABLE billing.subscriptions
    ADD COLUMN IF NOT EXISTS tier_group VARCHAR(100);

-- 2. Backfill from the owning product.
UPDATE billing.subscriptions AS sub
SET tier_group = prod.tier_group
FROM billing.products AS prod
WHERE prod.id = sub.product_id
  AND sub.tier_group IS DISTINCT FROM prod.tier_group;

-- 3. Keep tier_group in sync with the product whenever a subscription's
--    product_id is set or changed. The runtime sets product_id on every
--    insert/upgrade/downgrade, so this trigger fully maintains the column
--    without touching application code.
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

-- 4. PRE-EXISTING DUPLICATE HANDLING.
--    If two or more active/pending subscriptions already share the same
--    (user_id, tier_group), the unique index below would fail to build. We
--    cannot silently cancel a customer's subscription as part of a schema
--    migration (that has billing consequences), so instead we FAIL FAST with a
--    clear message listing the offending groups. An operator must dedup them
--    first (cancel/refund the redundant subscription via the normal billing
--    flow), then re-run the migration. The diagnostic query an operator can use
--    to find them is:
--
--      SELECT user_id, tier_group, count(*)
--      FROM billing.subscriptions
--      WHERE status IN ('active','pending') AND tier_group IS NOT NULL
--      GROUP BY user_id, tier_group HAVING count(*) > 1;
--
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
            'cannot enforce unique (user_id, tier_group): % active/pending duplicate tier-group group(s) exist; dedup them (cancel the redundant subscriptions) before applying this migration',
            dup_count;
    END IF;
END $$;

-- 5. The invariant: at most one active or pending subscription per
--    (user_id, tier_group). Cancelled / past_due rows are excluded so a user
--    can re-subscribe after cancelling, and tier_group IS NULL rows (products
--    with no tier group) are not constrained.
CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriptions_user_tier_group_active
    ON billing.subscriptions (user_id, tier_group)
    WHERE status IN ('active', 'pending') AND tier_group IS NOT NULL;

COMMENT ON COLUMN billing.subscriptions.tier_group IS
    'Denormalized from billing.products.tier_group (kept in sync by trigger trg_subscriptions_set_tier_group). Backs uq_subscriptions_user_tier_group_active, which enforces one active/pending subscription per (user, tier group).';
