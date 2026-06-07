SET lock_timeout      = '10s';
SET statement_timeout = '300s';

ALTER TABLE billing.entitlements
    DROP CONSTRAINT IF EXISTS entitlements_tenant_subject_no_overlap;
DROP INDEX IF EXISTS billing.uq_entitlements_tenant_subject_active;
DROP INDEX IF EXISTS billing.idx_entitlements_tenant_subject_active_window;
ALTER TABLE billing.entitlements
    DROP CONSTRAINT IF EXISTS entitlements_tenant_subject_fk;
ALTER TABLE billing.entitlements
    DROP COLUMN IF EXISTS tenant_subject_id;
