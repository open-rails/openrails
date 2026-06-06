SET lock_timeout      = '10s';
SET statement_timeout = '300s';

ALTER TABLE billing.tenant_delegated_issuers
    ADD COLUMN IF NOT EXISTS audiences TEXT[] NOT NULL DEFAULT '{}';

COMMENT ON COLUMN billing.tenant_delegated_issuers.audiences IS
    'Accepted OIDC JWT aud values for this tenant issuer.';
