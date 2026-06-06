-- =============================================================================
-- 039 — Tenant-aware core data model (issue #223, keystone)
--
-- Makes OpenRails core tenant-aware so one shared app/DB can serve many
-- tenants, while self-hosted single-tenant installs run the SAME code paths
-- with one DEFAULT tenant namespace.
--
-- HARDCUT / GREENFIELD: this branch replaces the pre-refactor single-tenant
-- schema wholesale. There are NO pre-existing rows to preserve, so tenant_id is
-- created NOT NULL from the start, the legacy non-tenant-scoped uniqueness
-- constraints are REPLACED (dropped + re-created tenant-scoped), and no
-- backfill-of-existing-rows logic is needed.
--
-- This migration:
--   * Creates billing.tenants (the tenant / billing-namespace directory).
--   * Seeds exactly one default tenant row (slug = 'default'). That default
--     tenant IS the self-hosted single-tenant namespace — it is NOT legacy.
--   * Adds a NOT NULL tenant_id column (defaulted to the 'default' tenant so
--     single-tenant writers that do not stamp a tenant land in the default
--     namespace) to every tenant-owned table.
--   * Replaces the legacy non-tenant-scoped unique constraints / indexes with
--     tenant-scoped ones (drops the old, adds the new — never both).
--   * Adds tenant-scoped lookup indexes for the hottest paths.
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

    -- Link to the OpenRails-owned AuthKit tenant that operates this tenant
    -- (control plane). Nullable until #221/#222 wire AuthKit tenant ownership.
    authkit_tenant_id   TEXT,
    authkit_tenant_slug TEXT,

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

CREATE UNIQUE INDEX IF NOT EXISTS uq_tenants_authkit_tenant_id
    ON billing.tenants (authkit_tenant_id)
    WHERE authkit_tenant_id IS NOT NULL;

COMMENT ON TABLE  billing.tenants IS
    'Tenant / billing-namespace directory. GLOBAL (control-plane) table, not tenant-scoped. Self-hosted installs have exactly one row (slug=default).';
COMMENT ON COLUMN billing.tenants.slug IS
    'Stable tenant slug used in tenant-scoped routes and resolution. The well-known value ''default'' is the single-tenant / self-hosted namespace.';
COMMENT ON COLUMN billing.tenants.authkit_tenant_id IS
    'OpenRails-owned AuthKit tenant id that operates this tenant (control plane). Nullable until org ownership is wired in #221/#222.';

-- -----------------------------------------------------------------------------
-- Seed the single DEFAULT tenant. Deterministic UUID so application code,
-- tests, and other services can rely on a well-known default tenant id.
--   default tenant id = 00000000-0000-0000-0000-000000000001
--
-- This default-tenant row IS the self-hosted single-tenant mode. It is the
-- legitimate single-tenant namespace, not a legacy compatibility shim.
-- -----------------------------------------------------------------------------

INSERT INTO billing.tenants (id, slug, name, status)
VALUES ('00000000-0000-0000-0000-000000000001', 'default', 'Default Tenant', 'active')
ON CONFLICT (slug) DO NOTHING;

-- -----------------------------------------------------------------------------
-- Add a NOT NULL tenant_id to every tenant-owned table. Greenfield: no existing
-- rows, so the column is created NOT NULL directly. It DEFAULTs to the 'default'
-- tenant so single-tenant writers that do not explicitly stamp a tenant land in
-- the default namespace (legitimate single-tenant mode), while multi-tenant
-- writers stamp the real tenant.
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
                'ALTER TABLE billing.%I ADD COLUMN IF NOT EXISTS tenant_id UUID NOT NULL DEFAULT %L',
                t, default_tenant
            );
            EXECUTE format(
                'CREATE INDEX IF NOT EXISTS idx_%I_tenant_id ON billing.%I (tenant_id)',
                t, t
            );
            EXECUTE format(
                $cmt$COMMENT ON COLUMN billing.%I.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.'$cmt$,
                t
            );
        END IF;
    END LOOP;
END $$;

-- -----------------------------------------------------------------------------
-- Replace the legacy non-tenant-scoped unique constraints / indexes with
-- tenant-scoped ones. HARDCUT: we DROP the old global uniques and CREATE the
-- tenant-scoped replacements — the two never coexist.
-- -----------------------------------------------------------------------------

-- payment_methods: (user_id, vault_id) and (processor, vault_id) -> tenant-scoped.
DROP INDEX IF EXISTS billing.uq_payment_methods_user_vault;
DROP INDEX IF EXISTS billing.idx_payment_methods_processor_vault_id;
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_methods_tenant_user_vault
    ON billing.payment_methods (tenant_id, user_id, vault_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_methods_tenant_processor_vault
    ON billing.payment_methods (tenant_id, processor, vault_id);

-- subscriptions: one lifecycle owner per (user, product) and processor-sub-id
-- uniqueness -> tenant-scoped.
DROP INDEX IF EXISTS billing.idx_subscriptions_user_product_lifecycle_owner;
DROP INDEX IF EXISTS billing.uniq_subscriptions_processor_subscription_id_nonempty;
CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriptions_tenant_user_product_lifecycle
    ON billing.subscriptions (tenant_id, user_id, product_id)
    WHERE status IN ('active', 'pending', 'past_due');
CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriptions_tenant_processor_subscription_id
    ON billing.subscriptions (tenant_id, processor, processor_subscription_id)
    WHERE processor_subscription_id <> '';

-- entitlements: at-most-one open-ended live entitlement per (user, entitlement)
-- and the no-overlap exclusion -> tenant-scoped.
DROP INDEX IF EXISTS billing.uniq_entitlements_active;
CREATE UNIQUE INDEX IF NOT EXISTS uq_entitlements_tenant_active
    ON billing.entitlements (tenant_id, user_id, entitlement)
    WHERE revoked_at IS NULL AND end_at IS NULL;

ALTER TABLE billing.entitlements DROP CONSTRAINT IF EXISTS entitlements_no_overlap;
ALTER TABLE billing.entitlements ADD CONSTRAINT entitlements_no_overlap
    EXCLUDE USING gist (
        tenant_id   WITH =,
        user_id     WITH =,
        entitlement WITH =,
        period      WITH &&
    )
    WHERE (revoked_at IS NULL AND deleted_at IS NULL);

-- payments: (processor, transaction_id) uniqueness -> tenant-scoped.
ALTER TABLE billing.payments DROP CONSTRAINT IF EXISTS payments_processor_transaction_unique;
CREATE UNIQUE INDEX IF NOT EXISTS uq_payments_tenant_processor_transaction
    ON billing.payments (tenant_id, processor, transaction_id);

-- processor_customers: (user_id, processor) and (processor, customer_id) -> tenant-scoped.
ALTER TABLE billing.processor_customers DROP CONSTRAINT IF EXISTS processor_customers_user_id_processor_key;
ALTER TABLE billing.processor_customers DROP CONSTRAINT IF EXISTS processor_customers_processor_customer_id_key;
CREATE UNIQUE INDEX IF NOT EXISTS uq_processor_customers_tenant_user_processor
    ON billing.processor_customers (tenant_id, user_id, processor);
CREATE UNIQUE INDEX IF NOT EXISTS uq_processor_customers_tenant_processor_customer
    ON billing.processor_customers (tenant_id, processor, customer_id);

-- -----------------------------------------------------------------------------
-- Tenant-scoped lookup indexes for the hottest read paths.
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
