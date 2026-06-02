-- Issue #232: tenant-scope ClickHouse analytics event tables.
--
-- Postgres remains the system of record for all billing state and money /
-- entitlement decisions. ClickHouse is a derived, tenant-scoped analytics sink.
-- Every active event row now carries tenant_id (the resolved tenant / billing
-- namespace) so hosted multi-tenant analytics can never be read across tenants.
--
-- tenant_id is the canonical default-tenant UUID for existing single-tenant
-- rows (matches pkg/tenant.DefaultID / migration 039's "default" tenant), so
-- backfilled historical rows are correctly attributed to the default tenant.

ALTER TABLE subscription_events {{ON_CLUSTER}}
    ADD COLUMN IF NOT EXISTS tenant_id String DEFAULT '00000000-0000-0000-0000-000000000001';

ALTER TABLE subscription_events {{ON_CLUSTER}}
    ADD INDEX IF NOT EXISTS idx_subscription_events_tenant (tenant_id) TYPE set(0) GRANULARITY 1;

ALTER TABLE payment_events {{ON_CLUSTER}}
    ADD COLUMN IF NOT EXISTS tenant_id String DEFAULT '00000000-0000-0000-0000-000000000001';

ALTER TABLE payment_events {{ON_CLUSTER}}
    ADD INDEX IF NOT EXISTS idx_payment_events_tenant (tenant_id) TYPE set(0) GRANULARITY 1;

ALTER TABLE acu_events {{ON_CLUSTER}}
    ADD COLUMN IF NOT EXISTS tenant_id String DEFAULT '00000000-0000-0000-0000-000000000001';

ALTER TABLE acu_events {{ON_CLUSTER}}
    ADD INDEX IF NOT EXISTS idx_acu_events_tenant (tenant_id) TYPE set(0) GRANULARITY 1;

ALTER TABLE chargeback_events {{ON_CLUSTER}}
    ADD COLUMN IF NOT EXISTS tenant_id String DEFAULT '00000000-0000-0000-0000-000000000001';

ALTER TABLE chargeback_events {{ON_CLUSTER}}
    ADD INDEX IF NOT EXISTS idx_chargeback_events_tenant (tenant_id) TYPE set(0) GRANULARITY 1;

-- daily_metrics gains tenant_id so admin rollups can be filtered per tenant.
-- The materialized view (002) is NOT changed here: per-tenant rollup of the MV,
-- and tenant-filtered AdminMetricsService queries plus the platform-superadmin
-- cross-tenant control-plane path, are tracked as remaining #232 work. Until
-- then daily_metrics rows default to the single default tenant, which is
-- correct for self-hosted / single-tenant installs.
ALTER TABLE daily_metrics {{ON_CLUSTER}}
    ADD COLUMN IF NOT EXISTS tenant_id String DEFAULT '00000000-0000-0000-0000-000000000001';
