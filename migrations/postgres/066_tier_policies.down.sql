-- Down migration for 066 — drop tier policies (issue #298).
SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP POLICY IF EXISTS tenant_isolation ON billing.tier_policies;
DROP TABLE IF EXISTS billing.tier_policies;
