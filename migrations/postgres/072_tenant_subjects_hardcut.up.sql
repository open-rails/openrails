-- =============================================================================
-- 072 — Hard-cut payable identity to tenant_subjects (#317)
--
-- OpenRails billing/payable identity is now a tenant subject:
--   tenant_subjects(id, tenant_id, issuer, subject, created_at, last_seen_at)
--
-- Billing tables reference tenant_subject_id. Callers must send
-- tenant_subject_id; old payer_org_id/account/delegated-user/subject-type fields
-- are not accepted by application code.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.tenant_subjects (
    id           UUID        PRIMARY KEY DEFAULT uuidv7(),
    tenant_id    UUID        NOT NULL REFERENCES billing.tenants(id),
    issuer       TEXT        NOT NULL,
    subject      TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_tenant_subjects_issuer_subject UNIQUE (tenant_id, issuer, subject)
);

CREATE INDEX IF NOT EXISTS idx_tenant_subjects_tenant
    ON billing.tenant_subjects (tenant_id);

COMMENT ON TABLE billing.tenant_subjects IS
    'OpenRails payable identity. One row per OIDC-style subject under an OpenRails tenant; billing tables reference this row.';
COMMENT ON COLUMN billing.tenant_subjects.issuer IS
    'OIDC issuer that asserted the subject.';
COMMENT ON COLUMN billing.tenant_subjects.subject IS
    'OIDC subject asserted by issuer. May represent a human, company, tenant, service, or chained delegated principal.';

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
            WHERE table_schema = 'billing' AND table_name = t AND column_name = 'payer_org_id'
        ) THEN
            EXECUTE format('ALTER TABLE billing.%I RENAME COLUMN payer_org_id TO tenant_subject_id', t);
        END IF;
    END LOOP;

    FOREACH t IN ARRAY user_subject_tables LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = 'billing' AND table_name = t AND column_name = 'user_id'
        ) THEN
            EXECUTE format('ALTER TABLE billing.%I RENAME COLUMN user_id TO tenant_subject_id', t);
        END IF;
    END LOOP;
END $$;

DO $$
DECLARE
    rec RECORD;
BEGIN
    FOR rec IN
        SELECT table_name
          FROM information_schema.columns
         WHERE table_schema = 'billing'
           AND column_name = 'tenant_subject_id'
    LOOP
        EXECUTE format(
            $sql$COMMENT ON COLUMN billing.%I.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.'$sql$,
            rec.table_name
        );
    END LOOP;
END $$;

CREATE INDEX IF NOT EXISTS idx_entitlements_tenant_subject
    ON billing.entitlements (tenant_subject_id, entitlement)
    WHERE revoked_at IS NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_subscriptions_tenant_subject
    ON billing.subscriptions (tenant_subject_id, product_id, status);
CREATE INDEX IF NOT EXISTS idx_payments_tenant_subject
    ON billing.payments (tenant_subject_id, purchased_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_events_tenant_subject_time
    ON billing.usage_events (tenant_subject_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_invoices_tenant_subject
    ON billing.invoices (tenant_subject_id, period_from DESC);
