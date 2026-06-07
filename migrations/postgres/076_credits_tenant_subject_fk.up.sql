-- =============================================================================
-- 076 - Reconcile credit/usage tenant_subject_id to real tenant_subjects rows (#317)
--
-- Migration 071 renamed the credit/usage/invoice/budget owner columns to
-- tenant_subject_id, but those values are the credits/self-service DETERMINISTIC
-- subject UUID (identity.TenantSubjectIDFromString(user.ID) == the user UUID),
-- with NO billing.tenant_subjects row and NO foreign key. The commerce hard cut
-- (074/075) keys the SAME self-service subjects to tenant_subjects rows whose id
-- equals that UUID (issuer 'openrails:self'). This migration converges the two:
--
--   1. For every distinct tenant_subject_id across the credit/usage/invoice/budget
--      tables that is NOT already a tenant_subjects row, materialize one whose id
--      IS that UUID (issuer 'openrails:self', subject the UUID text). Federated
--      subjects already have a tenant_subjects row (created at delegated-auth time)
--      and are skipped by the NOT EXISTS guard.
--   2. Add a foreign key from each table's tenant_subject_id to
--      billing.tenant_subjects(id).
--
-- After this, every billing table's tenant_subject_id is a real, joinable payable
-- identity. There are NO compatibility views.
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
        -- 1. Materialize a tenant_subjects row (id = the deterministic subject UUID)
        --    for every self-service subject that does not yet have one.
        EXECUTE format($q$
            INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
            SELECT DISTINCT t.tenant_subject_id, t.tenant_id, 'openrails:self', t.tenant_subject_id::text
              FROM billing.%I t
             WHERE t.tenant_subject_id IS NOT NULL
               AND NOT EXISTS (
                    SELECT 1 FROM billing.tenant_subjects tsub
                     WHERE tsub.id = t.tenant_subject_id
               )
            ON CONFLICT DO NOTHING
        $q$, tbl);

        -- 2. Foreign key to the payable identity table.
        EXECUTE format($q$
            DO $inner$
            BEGIN
                IF NOT EXISTS (
                    SELECT 1 FROM pg_constraint
                     WHERE conname = '%1$s_tenant_subject_fk'
                       AND conrelid = 'billing.%1$s'::regclass
                ) THEN
                    ALTER TABLE billing.%1$s
                        ADD CONSTRAINT %1$s_tenant_subject_fk
                        FOREIGN KEY (tenant_subject_id)
                        REFERENCES billing.tenant_subjects(id);
                END IF;
            END $inner$
        $q$, tbl);
    END LOOP;
END $$;
