-- Down migration for 064 — drop append-only usage events (issue #289).
SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP POLICY IF EXISTS tenant_isolation ON billing.usage_events;
DROP TABLE IF EXISTS billing.usage_events;
