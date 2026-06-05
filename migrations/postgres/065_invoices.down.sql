-- Down migration for 065 — drop monthly itemized invoices (issue #303).
SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP POLICY IF EXISTS tenant_isolation ON billing.invoices;
DROP TABLE IF EXISTS billing.invoices;
