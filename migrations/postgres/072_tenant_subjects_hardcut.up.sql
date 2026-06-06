-- =============================================================================
-- 072 — Hard-cut payable identity to tenant_subjects (#317)
--
-- OpenRails billing/payable identity is now a tenant subject:
--   tenant_subjects(id, tenant_id, issuer, subject, created_at, last_seen_at)
--
-- Credit/account/budget/usage/invoice tables reference tenant_subject_id. Callers
-- must send tenant_subject_id; old tenant_subject_id/account/delegated-user/
-- subject-type fields are not accepted by application code.
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

CREATE INDEX IF NOT EXISTS idx_usage_events_tenant_subject_time
    ON billing.usage_events (tenant_subject_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_invoices_tenant_subject
    ON billing.invoices (tenant_subject_id, period_from DESC);
