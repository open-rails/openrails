-- Reverse of 039: remove the tenant-aware core data model.
-- NOTE: migratekit (LoadFromFS) only applies *.up.sql files; this .down.sql is
-- kept for documentation / manual rollback and is NOT auto-loaded.
--
-- Drops tenant-scoped indexes, the tenant_id column on every tenant-owned
-- table, and finally the billing.tenants directory. Safe to run only if no
-- code path depends on tenant_id (i.e. before/without the tenant-aware query
-- layer being enabled).

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP INDEX IF EXISTS billing.idx_entitlements_tenant_user_ent;
DROP INDEX IF EXISTS billing.idx_user_credit_balances_tenant_user;
DROP INDEX IF EXISTS billing.idx_credit_transactions_tenant_user;
DROP INDEX IF EXISTS billing.idx_credit_blocks_tenant_user;

DO $$
DECLARE
    t TEXT;
    tenant_owned_tables CONSTANT TEXT[] := ARRAY[
        'products',
        'prices',
        'catalog_drift_events',
        'payment_methods',
        'subscriptions',
        'entitlements',
        'payments',
        'admin_grants',
        'notification_queue',
        'processor_customers',
        'credit_types',
        'credit_transactions',
        'credit_blocks',
        'user_credit_balances',
        'checkout_sessions',
        'manual_rebill_attempts'
    ];
BEGIN
    FOREACH t IN ARRAY tenant_owned_tables LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.tables
            WHERE table_schema = 'billing' AND table_name = t
        ) THEN
            EXECUTE format('DROP INDEX IF EXISTS billing.idx_%I_tenant_id', t);
            EXECUTE format('ALTER TABLE billing.%I DROP COLUMN IF EXISTS tenant_id', t);
        END IF;
    END LOOP;
END $$;

DROP TABLE IF EXISTS billing.tenants;
