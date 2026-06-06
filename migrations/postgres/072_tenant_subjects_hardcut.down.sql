SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP INDEX IF EXISTS billing.idx_invoices_tenant_subject;
DROP INDEX IF EXISTS billing.idx_usage_events_tenant_subject_time;

DO $$
DECLARE
    t TEXT;
    payable_tables CONSTANT TEXT[] := ARRAY[
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
BEGIN
    FOREACH t IN ARRAY payable_tables LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = 'billing' AND table_name = t AND column_name = 'tenant_subject_id'
        ) THEN
            EXECUTE format('ALTER TABLE billing.%I RENAME COLUMN tenant_subject_id TO tenant_subject_id', t);
        END IF;
    END LOOP;
END $$;

DROP TABLE IF EXISTS billing.tenant_subjects;
