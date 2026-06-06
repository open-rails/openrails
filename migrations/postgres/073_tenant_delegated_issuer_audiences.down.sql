SET lock_timeout = '10s';

ALTER TABLE billing.tenant_delegated_issuers
    DROP COLUMN IF EXISTS audiences;
