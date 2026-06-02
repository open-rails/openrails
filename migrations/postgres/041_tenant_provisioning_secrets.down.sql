-- Reverse of 041: remove tenant provisioning / secret-store state (issue #225).
-- NOTE: migratekit (LoadFromFS) only applies *.up.sql files; this .down.sql is
-- kept for documentation / manual rollback and is NOT auto-loaded.

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP TABLE IF EXISTS billing.tenant_exports;
DROP TABLE IF EXISTS billing.tenant_credential_audit;
DROP TABLE IF EXISTS billing.tenant_secrets;

DROP INDEX IF EXISTS billing.uq_tenants_webhook_host;

ALTER TABLE billing.tenants DROP COLUMN IF EXISTS provisioned_at;
ALTER TABLE billing.tenants DROP COLUMN IF EXISTS webhook_path;
ALTER TABLE billing.tenants DROP COLUMN IF EXISTS webhook_host;
ALTER TABLE billing.tenants DROP COLUMN IF EXISTS stripe_account_id;
ALTER TABLE billing.tenants DROP COLUMN IF EXISTS billing_tier;
