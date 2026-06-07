-- =============================================================================
-- 078 - Drop user_id from payable billing tables (#317 hard cut, final step)
--
-- All readers and writers now reference billing.<table>.tenant_subject_id (the
-- payable tenant subject FK, backfilled by 074/075 and unified by 077). This
-- migration recreates the user_id-based UNIQUE indexes on tenant_subject_id,
-- makes tenant_subject_id NOT NULL, and drops the user_id column. There are NO
-- compatibility views and NO fallback columns.
--
-- The entitlements tenant_subject_id unique + no-overlap exclusion already exist
-- (migration 074); the user_id-based ones (039) are removed by DROP COLUMN
-- CASCADE here. All non-unique idx_*_user_id lookup indexes are likewise dropped
-- by CASCADE — their tenant_subject_id replacements exist (074/075).
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '600s';

-- 1. Recreate the user_id-based UNIQUE indexes on tenant_subject_id (the column
--    is dropped with CASCADE below, which would otherwise silently drop the
--    business uniqueness guarantees).

-- payment_methods: one stored method per (tenant, subject, vault id).
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_methods_tenant_subject_vault
    ON billing.payment_methods (tenant_id, tenant_subject_id, vault_id);

-- subscriptions: one lifecycle owner per (tenant, subject, product).
CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriptions_tenant_subject_product_lifecycle
    ON billing.subscriptions (tenant_id, tenant_subject_id, product_id)
    WHERE status IN ('active', 'pending', 'past_due');

-- subscriptions: one active/pending subscription per (subject, tier group).
CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriptions_tenant_subject_tier_group_active
    ON billing.subscriptions (tenant_subject_id, tier_group)
    WHERE status IN ('active', 'pending') AND tier_group IS NOT NULL;

-- processor_customers: one processor customer per (tenant, subject, processor).
CREATE UNIQUE INDEX IF NOT EXISTS uq_processor_customers_tenant_subject_processor
    ON billing.processor_customers (tenant_id, tenant_subject_id, processor);

-- 2. tenant_subject_id is now the mandatory payable identity on every payable
--    table (backfilled for all existing rows; writers always set it).
DO $$
DECLARE
    tbl TEXT;
    payable_tables TEXT[] := ARRAY[
        'subscriptions',
        'payments',
        'payment_methods',
        'processor_customers',
        'checkout_sessions',
        'product_access_grants',
        'admin_grants',
        'notification_queue',
        'entitlements'
    ];
BEGIN
    FOREACH tbl IN ARRAY payable_tables
    LOOP
        EXECUTE format('ALTER TABLE billing.%I ALTER COLUMN tenant_subject_id SET NOT NULL', tbl);
    END LOOP;

    -- 3. Drop user_id. CASCADE removes the residual user_id-based indexes and
    --    constraints (idx_*_user_id, uq_subscriptions_user_tier_group_active,
    --    uq_payment_methods_tenant_user_vault, uq_subscriptions_tenant_user_product_lifecycle,
    --    uq_processor_customers_tenant_user_processor, uq_entitlements_tenant_active,
    --    entitlements_no_overlap, ...).
    FOREACH tbl IN ARRAY payable_tables
    LOOP
        EXECUTE format('ALTER TABLE billing.%I DROP COLUMN IF EXISTS user_id CASCADE', tbl);
    END LOOP;
END $$;
