-- Treat past_due subscriptions as lifecycle owners for tier-group uniqueness.
-- Processor-owned dunning can still recover a past_due subscription, so checkout
-- must not create a parallel subscription in the same tier group.

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

UPDATE billing.subscriptions AS sub
SET tier_group = prod.tier_group
FROM billing.products AS prod
WHERE prod.id = sub.product_id
  AND sub.tier_group IS DISTINCT FROM prod.tier_group;

DO $$
DECLARE
    dup_count integer;
BEGIN
    SELECT count(*) INTO dup_count FROM (
        SELECT 1
        FROM billing.subscriptions
        WHERE status IN ('active', 'pending', 'past_due')
          AND tier_group IS NOT NULL
        GROUP BY user_id, tier_group
        HAVING count(*) > 1
    ) AS dups;

    IF dup_count > 0 THEN
        RAISE EXCEPTION
            'cannot enforce unique (user_id, tier_group): % active/pending/past_due duplicate tier-group group(s) exist; dedup them before applying this migration',
            dup_count;
    END IF;
END $$;

DROP INDEX IF EXISTS billing.uq_subscriptions_user_tier_group_active;

CREATE UNIQUE INDEX uq_subscriptions_user_tier_group_active
    ON billing.subscriptions (user_id, tier_group)
    WHERE status IN ('active', 'pending', 'past_due') AND tier_group IS NOT NULL;

COMMENT ON INDEX billing.uq_subscriptions_user_tier_group_active IS
    'Enforces one lifecycle-owning subscription per (user, tier group), including past_due subscriptions under processor dunning.';
