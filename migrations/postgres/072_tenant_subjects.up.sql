-- =============================================================================
-- 072 — Tenant subjects as OpenRails payable identity
--
-- OpenRails has tenants, but it does not own native users. The payable/billable
-- entity is an OIDC-style subject asserted by a tenant issuer. A subject can be a
-- person, company/tenant, service, or chained delegated principal upstream; billing
-- tables reference tenant_subject_id and join here for tenant/issuer/subject.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.tenant_subjects (
    id           UUID        PRIMARY KEY DEFAULT uuidv7(),
    tenant_id    UUID        NOT NULL REFERENCES billing.tenants(id) ON DELETE CASCADE,
    issuer       TEXT        NOT NULL,
    subject      TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT tenant_subjects_issuer_len_chk CHECK (char_length(issuer) BETWEEN 1 AND 512),
    CONSTRAINT tenant_subjects_subject_len_chk CHECK (char_length(subject) BETWEEN 1 AND 512),
    CONSTRAINT uq_tenant_subjects_identity UNIQUE (tenant_id, issuer, subject)
);

CREATE INDEX IF NOT EXISTS idx_tenant_subjects_tenant
    ON billing.tenant_subjects (tenant_id);

COMMENT ON TABLE billing.tenant_subjects IS
    'OIDC-style payable subjects for OpenRails billing. Stores tenant + issuer + subject once; billing tables reference tenant_subject_id.';
COMMENT ON COLUMN billing.tenant_subjects.issuer IS
    'OIDC iss value for the external principal source.';
COMMENT ON COLUMN billing.tenant_subjects.subject IS
    'OIDC sub value as asserted by issuer. May represent a human, company/tenant, service, or chained delegated principal upstream.';

ALTER TABLE billing.tenant_subjects ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.tenant_subjects FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON billing.tenant_subjects;
CREATE POLICY tenant_isolation ON billing.tenant_subjects
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON billing.tenant_subjects TO openrails_app;
