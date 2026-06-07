-- =============================================================================
-- 076 (down) - drop credit/usage tenant_subject_id foreign keys (#317)
--
-- Only the FK constraints are removed; the tenant_subjects rows materialized by
-- the up migration are left in place (other tables reference them).
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DO $$
DECLARE
    tbl TEXT;
    credit_tables TEXT[] := ARRAY[
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
    FOREACH tbl IN ARRAY credit_tables
    LOOP
        EXECUTE format('ALTER TABLE billing.%1$s DROP CONSTRAINT IF EXISTS %1$s_tenant_subject_fk', tbl);
    END LOOP;
END $$;
