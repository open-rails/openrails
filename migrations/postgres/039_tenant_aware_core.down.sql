-- Reverse of 039: remove the tenant-aware core data model.
-- NOTE: migratekit (LoadFromFS) only applies *.up.sql files; this .down.sql is
-- kept for documentation / manual rollback and is NOT auto-loaded.
--
-- Restores the legacy non-tenant-scoped uniques, drops the tenant-scoped
-- replacements + tenant lookup indexes, drops the tenant_id column on every
-- tenant-owned table, and finally drops the billing.tenants directory.

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- Restore legacy non-tenant-scoped uniques and drop the tenant-scoped ones.
DROP INDEX IF EXISTS billing.uq_payment_methods_tenant_user_vault;
DROP INDEX IF EXISTS billing.uq_payment_methods_tenant_processor_vault;
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_methods_user_vault          ON billing.payment_methods(user_id, vault_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_methods_processor_vault_id ON billing.payment_methods(processor, vault_id);

DROP INDEX IF EXISTS billing.uq_subscriptions_tenant_user_product_lifecycle;
DROP INDEX IF EXISTS billing.uq_subscriptions_tenant_processor_subscription_id;
CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_user_product_lifecycle_owner
    ON billing.subscriptions(user_id, product_id)
    WHERE status IN ('active', 'pending', 'past_due');
CREATE UNIQUE INDEX IF NOT EXISTS uniq_subscriptions_processor_subscription_id_nonempty
    ON billing.subscriptions(processor, processor_subscription_id)
    WHERE processor_subscription_id <> '';

DROP INDEX IF EXISTS billing.uq_entitlements_tenant_active;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_entitlements_active
    ON billing.entitlements(user_id, entitlement)
    WHERE revoked_at IS NULL AND end_at IS NULL;
ALTER TABLE billing.entitlements DROP CONSTRAINT IF EXISTS entitlements_no_overlap;
ALTER TABLE billing.entitlements ADD CONSTRAINT entitlements_no_overlap
    EXCLUDE USING gist (user_id WITH =, entitlement WITH =, period WITH &&)
    WHERE (revoked_at IS NULL AND deleted_at IS NULL);

DROP INDEX IF EXISTS billing.uq_payments_tenant_processor_transaction;
ALTER TABLE billing.payments ADD CONSTRAINT payments_processor_transaction_unique UNIQUE (processor, transaction_id);

DROP INDEX IF EXISTS billing.uq_processor_customers_tenant_user_processor;
DROP INDEX IF EXISTS billing.uq_processor_customers_tenant_processor_customer;
ALTER TABLE billing.processor_customers ADD CONSTRAINT processor_customers_user_id_processor_key UNIQUE (user_id, processor);
ALTER TABLE billing.processor_customers ADD CONSTRAINT processor_customers_processor_customer_id_key UNIQUE (processor, customer_id);

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
