SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- Recreate service_jwt_grants (rollback of 081). Mirrors 079_service_jwt_grants.
CREATE TABLE IF NOT EXISTS billing.service_jwt_grants (
    id          UUID        PRIMARY KEY DEFAULT uuidv7(),
    tenant_id   UUID        NOT NULL REFERENCES billing.tenants (id) ON DELETE CASCADE,
    issuer      TEXT        NOT NULL,
    subject     TEXT        NOT NULL,
    permissions TEXT[]      NOT NULL DEFAULT '{}',
    resources   JSONB       NOT NULL DEFAULT '[]'::jsonb,
    enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,

    CONSTRAINT service_jwt_grants_identity_unique UNIQUE (tenant_id, issuer, subject),
    CONSTRAINT service_jwt_grants_issuer_len_chk CHECK (char_length(issuer) BETWEEN 1 AND 512),
    CONSTRAINT service_jwt_grants_subject_len_chk CHECK (char_length(subject) BETWEEN 1 AND 256)
);

CREATE INDEX IF NOT EXISTS service_jwt_grants_lookup_idx
    ON billing.service_jwt_grants (tenant_id, issuer, subject)
    WHERE enabled;

GRANT SELECT, INSERT, UPDATE, DELETE ON billing.service_jwt_grants TO openrails_app;
