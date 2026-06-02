-- =============================================================================
-- 041 — Tenant provisioning, lifecycle, processor credentials & webhook routing
--       (issue #225, builds on #223 migration 039)
--
-- Extends the billing.tenants directory created by 039 with the provisioning /
-- lifecycle / routing state issue #225 needs, and adds the two control-plane
-- tables that back the per-tenant secret store and credential audit log.
--
-- This migration is ADDITIVE and BACKWARD-COMPATIBLE:
--   * Adds nullable provisioning/routing columns to billing.tenants.
--   * Creates billing.tenant_secrets (DB-backed TenantSecretStore implementation
--     so the secret abstraction builds and tests WITHOUT a live Vault).
--   * Creates billing.tenant_credential_audit (rotation / test audit log).
--   * Creates billing.tenant_exports (export-before-delete bookkeeping; delete is
--     gated on a completed export).
--
-- The DB-backed secret store is the dev/self-hosted default. A managed deployment
-- swaps in the Vault-backed TenantSecretStore (see internal/tenancy/secrets_vault.go)
-- WITHOUT a schema change: these tables simply go unused in that mode.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- -----------------------------------------------------------------------------
-- Provisioning / routing / tier columns on the tenant directory.
-- billing_tier is the platform's OWN billing tier for this tenant (dogfood):
-- separate from `plan` (added in 039 as free-form hosting metadata).
-- webhook_host / webhook_path let an ingress map an inbound webhook URL to a
-- tenant; OpenRails still verifies the signature AFTER tenant resolution, so the
-- router is not the trust boundary.
-- -----------------------------------------------------------------------------

ALTER TABLE billing.tenants ADD COLUMN IF NOT EXISTS billing_tier      TEXT;
ALTER TABLE billing.tenants ADD COLUMN IF NOT EXISTS stripe_account_id TEXT;
ALTER TABLE billing.tenants ADD COLUMN IF NOT EXISTS webhook_host      TEXT;
ALTER TABLE billing.tenants ADD COLUMN IF NOT EXISTS webhook_path      TEXT;
ALTER TABLE billing.tenants ADD COLUMN IF NOT EXISTS provisioned_at    TIMESTAMPTZ;

COMMENT ON COLUMN billing.tenants.billing_tier IS
    'The platform''s OWN billing tier for this tenant (eats own dogfood, issue #225). Distinct from plan (free-form hosting metadata).';
COMMENT ON COLUMN billing.tenants.webhook_host IS
    'Optional host an ingress uses to route inbound webhooks to this tenant. OpenRails verifies the signature AFTER tenant resolution (router is not the trust boundary).';

-- A tenant may be reached by a routing host; keep it unique when set so two
-- tenants cannot claim the same webhook host.
CREATE UNIQUE INDEX IF NOT EXISTS uq_tenants_webhook_host
    ON billing.tenants (lower(webhook_host))
    WHERE webhook_host IS NOT NULL;

-- -----------------------------------------------------------------------------
-- billing.tenant_secrets — DB-backed per-tenant secret store.
--
-- One row per (tenant_id, name). `name` is a namespaced secret key, e.g.
-- "stripe/secret_key", "stripe/webhook_signing_secret". Values are stored as-is
-- here (DB-backed dev/self-hosted implementation); the Vault-backed store keeps
-- the SAME (tenant, name) addressing but holds the value in Vault under a
-- tenant-scoped path. This table is GLOBAL control-plane state (it IS the
-- per-tenant secret directory), not a tenant-owned table.
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenant_secrets (
    tenant_id   UUID        NOT NULL,
    name        TEXT        NOT NULL,
    value       TEXT        NOT NULL,
    version     INTEGER     NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,

    CONSTRAINT pk_tenant_secrets PRIMARY KEY (tenant_id, name)
);

COMMENT ON TABLE billing.tenant_secrets IS
    'DB-backed per-tenant secret store (issue #225). Namespaced by (tenant_id, name). The Vault-backed store keeps the same addressing but holds values in Vault. GLOBAL control-plane table.';

-- -----------------------------------------------------------------------------
-- billing.tenant_credential_audit — rotation / test audit log.
-- Records every put/rotate/test of a tenant secret for compliance. Append-only.
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenant_credential_audit (
    id          UUID        PRIMARY KEY DEFAULT uuidv7(),
    tenant_id   UUID        NOT NULL,
    name        TEXT        NOT NULL,
    action      TEXT        NOT NULL
                    CHECK (action IN ('put', 'rotate', 'delete', 'test')),
    actor       TEXT,
    detail      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_tenant_credential_audit_tenant
    ON billing.tenant_credential_audit (tenant_id, created_at DESC);

COMMENT ON TABLE billing.tenant_credential_audit IS
    'Append-only audit log of per-tenant credential put/rotate/delete/test events (issue #225).';

-- -----------------------------------------------------------------------------
-- billing.tenant_exports — export-before-delete bookkeeping.
-- Tenant deletion is gated on a completed export row, satisfying the
-- export-before-delete requirement for GDPR / portability.
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenant_exports (
    id           UUID        PRIMARY KEY DEFAULT uuidv7(),
    tenant_id    UUID        NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'completed'
                     CHECK (status IN ('pending', 'completed', 'failed')),
    location     TEXT,
    row_counts   JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    completed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_tenant_exports_tenant
    ON billing.tenant_exports (tenant_id, created_at DESC);

COMMENT ON TABLE billing.tenant_exports IS
    'Tenant logical-export bookkeeping (issue #225). Tenant deletion is gated on a completed export row (export-before-delete).';
