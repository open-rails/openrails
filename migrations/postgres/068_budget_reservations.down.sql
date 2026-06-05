-- Down migration for 068 — drop rolling-window money-budget reservations (issue #304).
SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP POLICY IF EXISTS tenant_isolation ON billing.budget_reservations;
DROP TABLE IF EXISTS billing.budget_reservations;
