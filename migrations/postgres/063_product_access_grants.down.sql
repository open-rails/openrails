-- Down migration for 063 — drop durable product access grants (issue #250).
SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP POLICY IF EXISTS tenant_isolation ON billing.product_access_grants;
DROP TABLE IF EXISTS billing.product_access_grants;
