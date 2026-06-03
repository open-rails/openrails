-- =============================================================================
-- 063 — Durable product access grants (issue #250)
--
-- Today a one-time product purchase creates an immutable billing.payments row and
-- one or more temporal billing.entitlements windows. Entitlements model FEATURE
-- access ("premium", "api_access") and are derived from Product.EntitlementsSpec.
-- They do NOT give a host app a clean "does this user OWN product X?" answer
-- without walking payment history + product metadata.
--
-- This migration adds billing.product_access_grants: durable, first-class
-- application-facing ownership/access of a specific PRODUCT (a purchased movie,
-- downloadable asset, model pack, etc.). It is DISTINCT from feature entitlements:
-- a product may carry EntitlementsSpec and/or CreditsSpec AND produce a grant.
--
-- RLS (issue #227): tenant-owned -> ENABLE + FORCE row level security with the
-- exact migration-050 tenant_isolation policy form, and DML grants for the
-- unprivileged openrails_app application role.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.product_access_grants (
    id           UUID        PRIMARY KEY DEFAULT uuidv7(),

    -- Tenant scoping (issue #223 / #227). NOT NULL: every grant belongs to a tenant.
    tenant_id    UUID        NOT NULL,

    -- Principal: the application user that owns/has access to the product.
    user_id      TEXT        NOT NULL,

    -- The owned product. References the catalog so a grant cannot dangle.
    product_id   UUID        NOT NULL REFERENCES billing.products(id),

    -- Where this grant came from: a one-time purchase, a subscription, or an
    -- admin/comp grant.
    source_type  TEXT        NOT NULL CHECK (source_type IN ('purchase', 'subscription', 'admin')),

    -- Free-form source reference used for idempotency (e.g. payment id, admin
    -- grant id, subscription id rendered as text). Empty string when not
    -- applicable; kept non-null so the idempotency index treats it uniformly.
    source_id    TEXT        NOT NULL DEFAULT '',

    -- Optional structured link back to the payment that produced this grant, so
    -- refund/chargeback reversal can find + revoke the grant by payment id.
    payment_id   UUID,

    status       TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),

    -- Access window. ends_at NULL => indefinite / durable ownership.
    starts_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    ends_at      TIMESTAMPTZ,

    revoked_at    TIMESTAMPTZ,
    revoke_reason TEXT,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE billing.product_access_grants IS
    'Durable, application-facing product ownership/access (issue #250). Distinct from feature entitlements: answers "does this user own product X?" / "list products this user can access". A successful one-time purchase creates a grant; refunds/chargebacks/admin revocation revoke it.';

-- Idempotency: re-processing the same purchase/source event must NOT create a
-- duplicate grant for the same (tenant, user, product, source). source_id is
-- non-null (default '') so the index applies uniformly.
CREATE UNIQUE INDEX IF NOT EXISTS uq_product_access_grants_source
    ON billing.product_access_grants (tenant_id, user_id, product_id, source_id);

-- Common lookups: a user's library (sorted) and reverse lookup by payment for
-- refund/chargeback revocation.
CREATE INDEX IF NOT EXISTS ix_product_access_grants_user
    ON billing.product_access_grants (tenant_id, user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS ix_product_access_grants_payment
    ON billing.product_access_grants (payment_id) WHERE payment_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- RLS (issue #227): exact migration-050 tenant_isolation policy form. Rows are
-- visible/writable only when billing.product_access_grants.tenant_id equals the
-- per-transaction app.tenant_id GUC. FORCE makes it apply to the table owner too;
-- only superuser / BYPASSRLS roles bypass (the app connects as openrails_app,
-- which has neither). Fail-closed: no GUC => zero rows.
-- ---------------------------------------------------------------------------
ALTER TABLE billing.product_access_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.product_access_grants FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON billing.product_access_grants;
CREATE POLICY tenant_isolation ON billing.product_access_grants
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- DML for the unprivileged application role. (Migration 050 also sets DEFAULT
-- PRIVILEGES on the billing schema, but grant explicitly so this migration is
-- self-contained and order-independent.)
GRANT SELECT, INSERT, UPDATE, DELETE ON billing.product_access_grants TO openrails_app;
