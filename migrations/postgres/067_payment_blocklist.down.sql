-- Down migration for 067 — drop the tenant-scoped payment blocklist (issue #300).
SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP POLICY IF EXISTS tenant_isolation ON billing.payment_blocklist;
DROP TABLE IF EXISTS billing.payment_blocklist;
