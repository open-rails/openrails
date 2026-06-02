-- =============================================================================
-- 050 (down) — Drop per-tenant RLS, the application role, and the DEK table.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

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
            EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON billing.%I', t);
            EXECUTE format('ALTER TABLE billing.%I NO FORCE ROW LEVEL SECURITY', t);
            EXECUTE format('ALTER TABLE billing.%I DISABLE  ROW LEVEL SECURITY', t);
        END IF;
    END LOOP;
END $$;

DROP TABLE IF EXISTS billing.tenant_deks;

-- Revoke grants before dropping the role (role drop fails while grants exist).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'openrails_app') THEN
        ALTER DEFAULT PRIVILEGES IN SCHEMA billing
            REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM openrails_app;
        ALTER DEFAULT PRIVILEGES IN SCHEMA billing
            REVOKE USAGE, SELECT ON SEQUENCES FROM openrails_app;
        REVOKE ALL ON ALL TABLES    IN SCHEMA billing FROM openrails_app;
        REVOKE ALL ON ALL SEQUENCES IN SCHEMA billing FROM openrails_app;
        REVOKE USAGE ON SCHEMA billing FROM openrails_app;
        DROP ROLE openrails_app;
    END IF;
END $$;
