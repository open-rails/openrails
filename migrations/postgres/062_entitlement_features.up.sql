-- =============================================================================
-- 062 — Stripe-shaped entitlement features + product features (issue #245)
--
-- Adds a first-class "feature" concept ON TOP of OpenRails' existing temporal
-- entitlement-window ledger (billing.entitlements). Today features are implicit
-- string keys inside products.entitlements_spec (a JSONB map). This migration
-- introduces Stripe's shape:
--
--   Feature  (billing.entitlement_features)            — a named, lookup-keyed
--             access feature (e.g. `premium`, `api_access`). lookup_key is the
--             stable value carried in AuthKit JWT entitlements / host-app checks.
--
--   ProductFeature (billing.product_entitlement_features) — attaches a feature to
--             a product (Stripe's product_feature), with an optional duration_days
--             granted when the product is purchased (null = indefinite).
--
-- The internal window ledger (billing.entitlements) remains the SOURCE OF TRUTH
-- for active/history access; these tables are the Stripe-shaped read/write layer.
-- products.entitlements_spec is intentionally KEPT working (no backfill here —
-- greenfield).
--
-- Both tables are tenant-owned, so per issue #227 they get FORCE ROW LEVEL
-- SECURITY + the tenant_isolation policy and explicit grants to openrails_app
-- (the unprivileged request-path role), mirroring migration 050.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- -----------------------------------------------------------------------------
-- billing.entitlement_features — first-class feature definitions.
-- lookup_key is unique per tenant and is the stable string used in claims/checks.
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS billing.entitlement_features (
    id          UUID        PRIMARY KEY DEFAULT uuidv7(),
    tenant_id   UUID        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    lookup_key  TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    active      BOOLEAN     NOT NULL DEFAULT true,
    metadata    JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT entitlement_features_tenant_lookup_key_key UNIQUE (tenant_id, lookup_key)
);

COMMENT ON TABLE billing.entitlement_features IS
    'Stripe-shaped first-class entitlement feature definitions (issue #245). lookup_key is the stable value carried in AuthKit JWT entitlements and host-app checks. The internal billing.entitlements window ledger remains the source of truth for active access.';

-- -----------------------------------------------------------------------------
-- billing.product_entitlement_features — attaches a feature to a product
-- (Stripe's product_feature). duration_days null = indefinite grant on purchase.
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS billing.product_entitlement_features (
    id                     UUID        PRIMARY KEY DEFAULT uuidv7(),
    tenant_id              UUID        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    product_id             UUID        NOT NULL REFERENCES billing.products(id) ON DELETE CASCADE,
    entitlement_feature_id UUID        NOT NULL REFERENCES billing.entitlement_features(id) ON DELETE CASCADE,
    duration_days          INTEGER,
    metadata               JSONB,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT product_entitlement_features_unique UNIQUE (tenant_id, product_id, entitlement_feature_id)
);

COMMENT ON TABLE billing.product_entitlement_features IS
    'Stripe-shaped product_feature attachments (issue #245): which entitlement features a product grants when purchased. duration_days null = indefinite.';

CREATE INDEX IF NOT EXISTS idx_product_entitlement_features_product
    ON billing.product_entitlement_features (tenant_id, product_id);
CREATE INDEX IF NOT EXISTS idx_product_entitlement_features_feature
    ON billing.product_entitlement_features (tenant_id, entitlement_feature_id);

-- -----------------------------------------------------------------------------
-- (issue #227) Per-tenant Row Level Security. FORCE so the policy applies even to
-- the table owner; only superuser / BYPASSRLS roles bypass (the app role has
-- neither). Fail-closed: an unset app.tenant_id GUC matches no rows. Mirrors the
-- exact pattern from migration 050.
-- -----------------------------------------------------------------------------
ALTER TABLE billing.entitlement_features ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.entitlement_features FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON billing.entitlement_features;
CREATE POLICY tenant_isolation ON billing.entitlement_features
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON billing.entitlement_features TO openrails_app;

ALTER TABLE billing.product_entitlement_features ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.product_entitlement_features FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON billing.product_entitlement_features;
CREATE POLICY tenant_isolation ON billing.product_entitlement_features
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON billing.product_entitlement_features TO openrails_app;
