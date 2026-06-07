-- =============================================================================
-- 075 (down) - drop commerce-table tenant-subject columns (#317)
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DO $$
DECLARE
    tbl TEXT;
    commerce_tables TEXT[] := ARRAY[
        'subscriptions',
        'payments',
        'payment_methods',
        'processor_customers',
        'checkout_sessions',
        'product_access_grants',
        'admin_grants',
        'notification_queue'
    ];
BEGIN
    FOREACH tbl IN ARRAY commerce_tables
    LOOP
        EXECUTE format('DROP INDEX IF EXISTS billing.idx_%s_tenant_subject', tbl);
        EXECUTE format('ALTER TABLE billing.%1$s DROP CONSTRAINT IF EXISTS %1$s_tenant_subject_fk', tbl);
        EXECUTE format('ALTER TABLE billing.%I DROP COLUMN IF EXISTS tenant_subject_id', tbl);
    END LOOP;
END $$;
