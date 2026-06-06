SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP INDEX IF EXISTS billing.idx_invoices_tenant_subject;
DROP INDEX IF EXISTS billing.idx_usage_events_tenant_subject_time;
DROP INDEX IF EXISTS billing.idx_payments_tenant_subject;
DROP INDEX IF EXISTS billing.idx_subscriptions_tenant_subject;
DROP INDEX IF EXISTS billing.idx_entitlements_tenant_subject;

DO $$
DECLARE
    t TEXT;
    payer_tables CONSTANT TEXT[] := ARRAY[
        'user_credit_balances',
        'credit_transactions',
        'credit_blocks',
        'credit_account_settings',
        'credit_spend_limits',
        'usage_events',
        'invoices',
        'tier_policies',
        'payment_blocklist',
        'budget_reservations'
    ];
    user_subject_tables CONSTANT TEXT[] := ARRAY[
        'entitlements',
        'subscriptions',
        'payments',
        'payment_methods',
        'processor_customers',
        'checkout_sessions',
        'manual_rebill_attempts',
        'product_access_grants'
    ];
BEGIN
    FOREACH t IN ARRAY payer_tables LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = 'billing' AND table_name = t AND column_name = 'tenant_subject_id'
        ) THEN
            EXECUTE format('ALTER TABLE billing.%I RENAME COLUMN tenant_subject_id TO payer_org_id', t);
        END IF;
    END LOOP;

    FOREACH t IN ARRAY user_subject_tables LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = 'billing' AND table_name = t AND column_name = 'tenant_subject_id'
        ) THEN
            EXECUTE format('ALTER TABLE billing.%I RENAME COLUMN tenant_subject_id TO user_id', t);
        END IF;
    END LOOP;
END $$;

DROP TABLE IF EXISTS billing.tenant_subjects;
