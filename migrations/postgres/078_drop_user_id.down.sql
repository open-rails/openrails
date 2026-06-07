-- =============================================================================
-- 078 (down) - re-add user_id columns (#317)
--
-- The hard cut is not data-reversible: the dropped user_id values are recovered
-- from billing.tenant_subjects.subject (for self-service identities the subject
-- IS the user UUID; federated subjects recover their original external subject).
-- The user_id-based unique indexes from migration 039 are NOT recreated here —
-- re-applying 039+ forward is the supported path if a full rollback is needed.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '600s';

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
        EXECUTE format('ALTER TABLE billing.%I ADD COLUMN IF NOT EXISTS user_id TEXT', tbl);
        EXECUTE format($q$
            UPDATE billing.%I t
               SET user_id = tsub.subject
              FROM billing.tenant_subjects tsub
             WHERE tsub.id = t.tenant_subject_id
               AND t.user_id IS NULL
        $q$, tbl);
        EXECUTE format('ALTER TABLE billing.%I ALTER COLUMN tenant_subject_id DROP NOT NULL', tbl);
    END LOOP;
END $$;

DROP INDEX IF EXISTS billing.uq_payment_methods_tenant_subject_vault;
DROP INDEX IF EXISTS billing.uq_subscriptions_tenant_subject_product_lifecycle;
DROP INDEX IF EXISTS billing.uq_subscriptions_tenant_subject_tier_group_active;
DROP INDEX IF EXISTS billing.uq_processor_customers_tenant_subject_processor;
