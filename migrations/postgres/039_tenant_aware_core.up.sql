-- =============================================================================
-- 039 — Tenant-aware core data model (issue #223, keystone)
--
-- Makes OpenRails core tenant-aware so one shared app/DB can serve many
-- tenants, while self-hosted single-tenant installs run the SAME code paths
-- with one DEFAULT tenant namespace.
--
-- This migration is intentionally ADDITIVE and BACKWARD-COMPATIBLE:
--   * Creates billing.tenants (the tenant / billing-namespace directory).
--   * Seeds exactly one default tenant row (slug = 'default').
--   * Adds a NULLABLE tenant_id column to every tenant-owned table.
--   * Backfills every existing row to the default tenant.
--   * Adds NEW tenant-scoped indexes alongside existing ones.
--
-- It does NOT (deferred to follow-up migrations once all queries are scoped):
--   * Enforce NOT NULL on tenant_id.
--   * Drop or rewrite any existing unique constraint / index.
--   * Add cross-table foreign keys to billing.tenants (kept loose so the
--     backfill + future per-tenant work can proceed incrementally).
--
-- CROSS-REPO COUPLING NOTE: doujins and hentai0 read billing.entitlements via
-- DIRECT SQL from their own processes. Adding a nullable column and extra
-- indexes is fully backward compatible with `SELECT ... FROM billing.entitlements
-- WHERE user_id = ...` style reads, so those direct readers keep working
-- unchanged. Do NOT make tenant_id NOT NULL or change entitlements' existing
-- unique/overlap constraints until those readers are coordinated.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- -----------------------------------------------------------------------------
-- billing.tenants — tenant / billing-namespace directory (control-plane state)
--
-- This table is itself GLOBAL (not tenant-scoped): it IS the tenant directory.
-- One row per tenant. Self-hosted deployments have exactly one row ('default').
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenants (
    id              UUID         PRIMARY KEY DEFAULT uuidv7(),
    slug            TEXT         NOT NULL,
    name            TEXT         NOT NULL,
    status          TEXT         NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'suspended', 'deleted')),

    -- Link to the OpenRails-owned AuthKit org that operates this tenant
    -- (control plane). Nullable until #221/#222 wire AuthKit org ownership.
    authkit_org_id   TEXT,
    authkit_org_slug TEXT,

    -- Optional hosting/plan/region metadata for the managed platform. Unused
    -- by self-hosted single-tenant installs.
    plan            TEXT,
    region          TEXT,

    created_at      TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    suspended_at    TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,

    CONSTRAINT uq_tenants_slug UNIQUE (slug)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_tenants_authkit_org_id
    ON billing.tenants (authkit_org_id)
    WHERE authkit_org_id IS NOT NULL;

COMMENT ON TABLE  billing.tenants IS
    'Tenant / billing-namespace directory. GLOBAL (control-plane) table, not tenant-scoped. Self-hosted installs have exactly one row (slug=default).';
COMMENT ON COLUMN billing.tenants.slug IS
    'Stable tenant slug used in tenant-scoped routes and resolution. The well-known value ''default'' is the single-tenant / self-hosted namespace.';
COMMENT ON COLUMN billing.tenants.authkit_org_id IS
    'OpenRails-owned AuthKit org id that operates this tenant (control plane). Nullable until org ownership is wired in #221/#222.';

-- -----------------------------------------------------------------------------
-- Seed the single DEFAULT tenant. Deterministic UUID so application code,
-- tests, and other services can rely on a well-known default tenant id.
--   default tenant id = 00000000-0000-0000-0000-000000000001
-- -----------------------------------------------------------------------------

INSERT INTO billing.tenants (id, slug, name, status)
VALUES ('00000000-0000-0000-0000-000000000001', 'default', 'Default Tenant', 'active')
ON CONFLICT (slug) DO NOTHING;

-- -----------------------------------------------------------------------------
-- Add a NULLABLE tenant_id to every tenant-owned table and backfill it to the
-- default tenant. Each block is idempotent (ADD COLUMN IF NOT EXISTS + guarded
-- backfill) so the migration is safe to reason about and re-read.
--
-- Tenant-owned tables (per issue #223):
--   products, prices, catalog_drift_events, payment_methods, subscriptions,
--   entitlements, payments, admin_grants, notification_queue,
--   processor_customers, credit_types, credit_transactions, credit_blocks,
--   user_credit_balances, checkout_sessions, manual_rebill_attempts.
--
-- Global tables (NOT given tenant_id): billing.tenants (this directory),
-- migrations bookkeeping, River job tables.
-- -----------------------------------------------------------------------------

DO $$
DECLARE
    default_tenant CONSTANT UUID := '00000000-0000-0000-0000-000000000001';
    t TEXT;
    tenant_owned_tables CONSTANT TEXT[] := ARRAY[
        'products',
        'prices',
        'catalog_drift_events',
        'payment_methods',
        'subscriptions',
        'entitlements',
        'payments',
        'admin_grants',
        'notification_queue',
        'processor_customers',
        'credit_types',
        'credit_transactions',
        'credit_blocks',
        'user_credit_balances',
        'checkout_sessions',
        'manual_rebill_attempts'
    ];
BEGIN
    FOREACH t IN ARRAY tenant_owned_tables LOOP
        -- Only touch tables that actually exist (defensive for partial schemas).
        IF EXISTS (
            SELECT 1 FROM information_schema.tables
            WHERE table_schema = 'billing' AND table_name = t
        ) THEN
            EXECUTE format(
                'ALTER TABLE billing.%I ADD COLUMN IF NOT EXISTS tenant_id UUID',
                t
            );
            EXECUTE format(
                'UPDATE billing.%I SET tenant_id = %L WHERE tenant_id IS NULL',
                t, default_tenant
            );
            EXECUTE format(
                'ALTER TABLE billing.%I ALTER COLUMN tenant_id SET DEFAULT %L',
                t, default_tenant
            );
            EXECUTE format(
                'CREATE INDEX IF NOT EXISTS idx_%I_tenant_id ON billing.%I (tenant_id)',
                t, t
            );
            EXECUTE format(
                $cmt$COMMENT ON COLUMN billing.%I.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). Nullable + defaulted to the ''default'' tenant during the additive rollout; will become NOT NULL once all queries are tenant-scoped.'$cmt$,
                t
            );
        END IF;
    END LOOP;
END $$;

-- -----------------------------------------------------------------------------
-- NEW tenant-scoped indexes for the hottest lookup paths. These sit ALONGSIDE
-- the existing user-scoped indexes/uniques (which are intentionally left in
-- place for backward compatibility, including direct-SQL readers). They give
-- the tenant-aware query layer efficient, tenant-prefixed access.
--
-- We do NOT add tenant-scoped UNIQUE replacements for the existing uniques yet,
-- because that requires retiring the old global uniques in lockstep with all
-- writers being tenant-aware. That is deferred to a follow-up migration.
-- -----------------------------------------------------------------------------

-- Entitlements: tenant + user + entitlement live-window lookups.
CREATE INDEX IF NOT EXISTS idx_entitlements_tenant_user_ent
    ON billing.entitlements (tenant_id, user_id, entitlement)
    WHERE revoked_at IS NULL AND deleted_at IS NULL;

-- Credit balances: tenant + user + type (the GetBalance hot path).
CREATE INDEX IF NOT EXISTS idx_user_credit_balances_tenant_user
    ON billing.user_credit_balances (tenant_id, user_id, credit_type_id);

-- Credit transactions: tenant + user + type history.
CREATE INDEX IF NOT EXISTS idx_credit_transactions_tenant_user
    ON billing.credit_transactions (tenant_id, user_id, credit_type_id, created_at DESC);

-- Credit blocks: tenant + user + type FIFO consumption.
CREATE INDEX IF NOT EXISTS idx_credit_blocks_tenant_user
    ON billing.credit_blocks (tenant_id, user_id, credit_type_id, expires_at ASC);
