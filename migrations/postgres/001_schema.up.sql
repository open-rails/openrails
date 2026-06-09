-- =============================================================================
-- OpenRails billing schema — consolidated single migration.
--
-- This file is the full structural state of the `billing` schema after all
-- historical Postgres migrations (001–082) have been applied. It is organized
-- for readability rather than as a history:
--   * Topological order (referenced tables defined before referencing tables).
--   * Primary keys, uniques, checks, indexes, RLS, and comments live next to
--     their table.
--   * Backfills, column renames, add-then-drop churn, and other one-time data
--     fixes from the original migrations are omitted — only the final structural
--     state remains.
--
-- Multi-tenancy (#223/#317): tenant-scoped tables carry tenant_id (RLS-isolated
-- via app.tenant_id) and, where they reference a payable identity,
-- tenant_subject_id -> billing.tenant_subjects. Control-plane/global tables
-- (tenants, tenant_*, platform_*) are NOT tenant-scoped and carry no RLS.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- -----------------------------------------------------------------------------
-- Extensions and schema
-- -----------------------------------------------------------------------------

CREATE EXTENSION IF NOT EXISTS btree_gist;   -- entitlements no-overlap EXCLUDE constraint

CREATE SCHEMA IF NOT EXISTS billing;

-- -----------------------------------------------------------------------------
-- Enums
-- -----------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
                   WHERE t.typname = 'processor_type' AND n.nspname = 'billing') THEN
        CREATE TYPE billing.processor_type AS ENUM (
            'paypal', 'solana', 'mobius', 'ccbill', 'stripe', 'admin', 'nmi', 'manual'
        );
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
                   WHERE t.typname = 'purchase_status' AND n.nspname = 'billing') THEN
        CREATE TYPE billing.purchase_status AS ENUM (
            'pending', 'completed', 'failed', 'refunded'
        );
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
                   WHERE t.typname = 'subscription_status' AND n.nspname = 'billing') THEN
        CREATE TYPE billing.subscription_status AS ENUM (
            'pending', 'active', 'expired', 'cancelled', 'failed', 'past_due'
        );
    END IF;
END$$;

-- =============================================================================
-- CONTROL PLANE (global, not tenant-scoped, no RLS)
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.tenants — tenant / billing-namespace directory
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenants (
    id                   UUID         PRIMARY KEY DEFAULT uuidv7(),
    slug                 TEXT         NOT NULL,
    name                 TEXT         NOT NULL,
    status               TEXT         NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended','deleted')),
    authkit_tenant_id    TEXT,
    authkit_tenant_slug  TEXT,
    plan                 TEXT,
    region               TEXT,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    suspended_at         TIMESTAMPTZ,
    deleted_at           TIMESTAMPTZ,
    billing_tier         TEXT,
    stripe_account_id    TEXT,
    webhook_host         TEXT,
    webhook_path         TEXT,
    provisioned_at       TIMESTAMPTZ,
    CONSTRAINT uq_tenants_slug UNIQUE (slug)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_tenants_authkit_tenant_id ON billing.tenants(authkit_tenant_id) WHERE authkit_tenant_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_tenants_webhook_host      ON billing.tenants(lower(webhook_host)) WHERE webhook_host IS NOT NULL;

-- Seed the single DEFAULT tenant (deterministic well-known id). This row IS the
-- self-hosted single-tenant namespace, not legacy data. tenant_id columns default
-- to it so single-tenant writers land in this namespace.
INSERT INTO billing.tenants (id, slug, name, status)
VALUES ('00000000-0000-0000-0000-000000000001', 'default', 'Default Tenant', 'active')
ON CONFLICT (slug) DO NOTHING;

COMMENT ON TABLE  billing.tenants                   IS 'Tenant / billing-namespace directory. GLOBAL (control-plane) table, not tenant-scoped. Self-hosted installs have exactly one row (slug=default).';
COMMENT ON COLUMN billing.tenants.slug              IS 'Stable tenant slug used in tenant-scoped routes and resolution. The well-known value ''default'' is the single-tenant / self-hosted namespace.';
COMMENT ON COLUMN billing.tenants.authkit_tenant_id IS 'OpenRails-owned AuthKit tenant id that operates this tenant (control plane). Nullable until org ownership is wired in #221/#222.';
COMMENT ON COLUMN billing.tenants.billing_tier      IS 'The platform''s OWN billing tier for this tenant (eats own dogfood, issue #225). Distinct from plan (free-form hosting metadata).';
COMMENT ON COLUMN billing.tenants.webhook_host      IS 'Optional host an ingress uses to route inbound webhooks to this tenant. OpenRails verifies the signature AFTER tenant resolution (router is not the trust boundary).';

-- -----------------------------------------------------------------------------
-- billing.tenant_subjects — OpenRails payable identity
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenant_subjects (
    id            UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id     UUID         NOT NULL REFERENCES billing.tenants(id),
    issuer        TEXT         NOT NULL,
    subject       TEXT         NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT uq_tenant_subjects_issuer_subject UNIQUE (tenant_id, issuer, subject)
);

CREATE INDEX IF NOT EXISTS idx_tenant_subjects_tenant ON billing.tenant_subjects(tenant_id);

COMMENT ON TABLE  billing.tenant_subjects         IS 'OpenRails payable identity. One row per OIDC-style subject under an OpenRails tenant; billing tables reference this row.';
COMMENT ON COLUMN billing.tenant_subjects.issuer  IS 'OIDC issuer that asserted the subject.';
COMMENT ON COLUMN billing.tenant_subjects.subject IS 'OIDC subject asserted by issuer. May represent a human, company, tenant, service, or chained delegated principal.';

-- -----------------------------------------------------------------------------
-- billing.tenant_deks — wrapped per-tenant Data Encryption Keys (#227)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenant_deks (
    tenant_id    UUID         NOT NULL,
    wrapped_dek  BYTEA        NOT NULL,
    key_version  INT          NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    CONSTRAINT pk_tenant_deks PRIMARY KEY (tenant_id)
);

COMMENT ON TABLE  billing.tenant_deks             IS 'Wrapped per-tenant Data Encryption Keys for envelope encryption-at-rest (issue #227). wrapped_dek = tenant DEK sealed with the master key (AES-256-GCM, nonce||ct||tag). Master key lives in config/env (self-hosted) or KMS (production), never in the DB. GLOBAL control-plane table.';
COMMENT ON COLUMN billing.tenant_deks.wrapped_dek IS 'AES-256-GCM(master_key, tenant_dek): nonce(12) || ciphertext(32) || tag(16).';

-- -----------------------------------------------------------------------------
-- billing.tenant_secrets — DB-backed per-tenant secret store (#225)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenant_secrets (
    tenant_id   UUID         NOT NULL,
    name        TEXT         NOT NULL,
    value       TEXT         NOT NULL,
    version     INT          NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    CONSTRAINT pk_tenant_secrets PRIMARY KEY (tenant_id, name)
);

COMMENT ON TABLE billing.tenant_secrets IS 'DB-backed per-tenant secret store (issue #225). Namespaced by (tenant_id, name). The Vault-backed store keeps the same addressing but holds values in Vault. GLOBAL control-plane table.';

-- -----------------------------------------------------------------------------
-- billing.tenant_credential_audit — per-tenant credential event log (#225)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenant_credential_audit (
    id          UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id   UUID         NOT NULL,
    name        TEXT         NOT NULL,
    action      TEXT         NOT NULL CHECK (action IN ('put','rotate','delete','test')),
    actor       TEXT,
    detail      TEXT,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_tenant_credential_audit_tenant ON billing.tenant_credential_audit(tenant_id, created_at DESC);

COMMENT ON TABLE billing.tenant_credential_audit IS 'Append-only audit log of per-tenant credential put/rotate/delete/test events (issue #225).';

-- -----------------------------------------------------------------------------
-- billing.tenant_exports — tenant logical-export bookkeeping (#225)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenant_exports (
    id            UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id     UUID         NOT NULL,
    status        TEXT         NOT NULL DEFAULT 'completed' CHECK (status IN ('pending','completed','failed')),
    location      TEXT,
    row_counts    JSONB,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    completed_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_tenant_exports_tenant ON billing.tenant_exports(tenant_id, created_at DESC);

COMMENT ON TABLE billing.tenant_exports IS 'Tenant logical-export bookkeeping (issue #225). Tenant deletion is gated on a completed export row (export-before-delete).';

-- -----------------------------------------------------------------------------
-- billing.tenant_delegated_issuers — federated delegated-token issuers (#259)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenant_delegated_issuers (
    id          UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id   UUID         NOT NULL REFERENCES billing.tenants(id) ON DELETE CASCADE,
    issuer      TEXT         NOT NULL,
    jwks_uri    TEXT         NOT NULL,
    enabled     BOOLEAN      NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    audiences   TEXT[]       NOT NULL DEFAULT '{}'::text[],
    CONSTRAINT uq_tenant_delegated_issuers_issuer UNIQUE (issuer)
);

CREATE INDEX IF NOT EXISTS idx_tenant_delegated_issuers_enabled ON billing.tenant_delegated_issuers(enabled) WHERE enabled;
CREATE INDEX IF NOT EXISTS idx_tenant_delegated_issuers_tenant  ON billing.tenant_delegated_issuers(tenant_id);

COMMENT ON TABLE  billing.tenant_delegated_issuers           IS 'Federated delegated-token issuer registry (issue #259). Maps a globally-unique token issuer (iss) to the OpenRails tenant it speaks for and the JWKS URL its public keys are fetched from. MANY issuers -> ONE tenant (multiple host apps = distinct keys, one tenant, shared users). GLOBAL control-plane table, not tenant-scoped.';
COMMENT ON COLUMN billing.tenant_delegated_issuers.issuer    IS 'Token iss value. GLOBALLY UNIQUE -> maps to exactly one tenant (no cross-tenant forgery).';
COMMENT ON COLUMN billing.tenant_delegated_issuers.jwks_uri  IS 'The ONLY URL OpenRails fetches this issuer''s keys from (allowlist; token-supplied URLs never honored).';
COMMENT ON COLUMN billing.tenant_delegated_issuers.enabled   IS 'Per-issuer kill-switch. Only enabled rows are loaded into the live verifier; disabling evicts without affecting sibling issuers of the same tenant.';
COMMENT ON COLUMN billing.tenant_delegated_issuers.audiences IS 'Accepted OIDC JWT aud values for this tenant issuer.';

-- -----------------------------------------------------------------------------
-- billing.platform_audit — cross-tenant superadmin audit log (#226)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.platform_audit (
    id               UUID         PRIMARY KEY DEFAULT uuidv7(),
    invoker_id    TEXT         NOT NULL,
    actor_org        TEXT,
    action           TEXT         NOT NULL,
    target_tenant_id UUID,
    reason           TEXT,
    before_state     JSONB,
    after_state      JSONB,
    detail           JSONB,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_platform_audit_action ON billing.platform_audit(action, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_platform_audit_actor  ON billing.platform_audit(invoker_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_platform_audit_target ON billing.platform_audit(target_tenant_id, created_at DESC);

COMMENT ON TABLE billing.platform_audit IS 'Append-only cross-tenant platform superadmin audit log (issue #226). Records actor, target tenant, action, reason, and before/after state. CROSS-TENANT control-plane state: NOT purged by tenant delete.';

-- -----------------------------------------------------------------------------
-- billing.platform_break_glass — time-boxed break-glass elevation (#226)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.platform_break_glass (
    id               UUID         PRIMARY KEY DEFAULT uuidv7(),
    invoker_id    TEXT         NOT NULL,
    target_tenant_id UUID,
    justification    TEXT         NOT NULL,
    granted_at       TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    expires_at       TIMESTAMPTZ  NOT NULL,
    revoked_at       TIMESTAMPTZ,
    CONSTRAINT chk_break_glass_justified CHECK (length(btrim(justification)) > 0),
    CONSTRAINT chk_break_glass_window    CHECK (expires_at > granted_at)
);

CREATE INDEX IF NOT EXISTS idx_break_glass_active ON billing.platform_break_glass(expires_at) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_break_glass_actor  ON billing.platform_break_glass(invoker_id, expires_at DESC);

COMMENT ON TABLE billing.platform_break_glass IS 'Time-boxed break-glass elevation grants (issue #226). Each grant carries a written justification and an expiry, and is mirrored into platform_audit. CROSS-TENANT control-plane state.';

-- =============================================================================
-- CATALOG
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.products — product catalog
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.products (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    slug               TEXT         NOT NULL,
    display_name       TEXT         NOT NULL,
    description        TEXT,
    entitlements_spec  JSONB,
    credits_spec       JSONB,
    tier_group         VARCHAR(100),
    tier_rank          INT          NOT NULL DEFAULT 0,
    status             TEXT         NOT NULL DEFAULT 'active' CHECK (status IN ('draft','active','archived')),
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    tenant_id          UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    CONSTRAINT products_slug_key UNIQUE (slug)
);

CREATE INDEX IF NOT EXISTS idx_products_slug       ON billing.products(slug);
CREATE INDEX IF NOT EXISTS idx_products_status     ON billing.products(status);
CREATE INDEX IF NOT EXISTS idx_products_tenant_id  ON billing.products(tenant_id);
CREATE INDEX IF NOT EXISTS idx_products_tier_group ON billing.products(tier_group) WHERE tier_group IS NOT NULL;

ALTER TABLE billing.products ENABLE  ROW LEVEL SECURITY;
ALTER TABLE billing.products FORCE   ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.products
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.products             IS 'Product definitions that can be purchased or subscribed to';
COMMENT ON COLUMN billing.products.credits_spec IS 'Bundled promo credits spec (amount, expiry, cadence) for subscriptions';
COMMENT ON COLUMN billing.products.tier_group   IS 'Semantic group name for mutually-exclusive products (e.g., "premium"). Products in same group require upgrade/downgrade, not parallel ownership.';
COMMENT ON COLUMN billing.products.tier_rank    IS 'Tier ranking within group. Higher = more premium. Used to determine upgrade (higher rank) vs downgrade (lower rank) direction.';
COMMENT ON COLUMN billing.products.tenant_id    IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';

-- -----------------------------------------------------------------------------
-- billing.prices — pricing tiers for products
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.prices (
    id                  UUID         PRIMARY KEY DEFAULT uuidv7(),
    product_id          UUID         NOT NULL REFERENCES billing.products(id) ON DELETE RESTRICT,
    amount              BIGINT       NOT NULL,
    currency            TEXT         NOT NULL,
    billing_cycle_days  INT,
    processors          JSONB,
    status              TEXT         NOT NULL DEFAULT 'active' CHECK (status IN ('draft','active','archived')),
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    tenant_id           UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    CONSTRAINT unique_prices_product_amount_cycle UNIQUE (product_id, amount, currency, billing_cycle_days)
);

CREATE INDEX IF NOT EXISTS idx_prices_processors ON billing.prices USING gin (processors);
CREATE INDEX IF NOT EXISTS idx_prices_product_id ON billing.prices(product_id);
CREATE INDEX IF NOT EXISTS idx_prices_status     ON billing.prices(status);
CREATE INDEX IF NOT EXISTS idx_prices_tenant_id  ON billing.prices(tenant_id);

ALTER TABLE billing.prices ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.prices FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.prices
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.prices           IS 'Pricing tiers for products with processor-specific identifiers';
COMMENT ON COLUMN billing.prices.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';

-- =============================================================================
-- CREDITS — credit "currencies"
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.credit_types
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.credit_types (
    id              UUID         PRIMARY KEY DEFAULT uuidv7(),
    name            TEXT         NOT NULL,
    display_name    TEXT         NOT NULL,
    unit            TEXT         NOT NULL DEFAULT 'usd',
    decimal_places  INT          NOT NULL DEFAULT 2,
    is_active       BOOLEAN      NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    tenant_id       UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    CONSTRAINT credit_types_name_key UNIQUE (name)
);

CREATE INDEX IF NOT EXISTS idx_credit_types_tenant_id ON billing.credit_types(tenant_id);

ALTER TABLE billing.credit_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.credit_types FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.credit_types
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON COLUMN billing.credit_types.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';

-- =============================================================================
-- ENTITLEMENT FEATURES (Stripe-shaped, #245)
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.entitlement_features
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.entitlement_features (
    id          UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id   UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    lookup_key  TEXT         NOT NULL,
    name        TEXT         NOT NULL,
    active      BOOLEAN      NOT NULL DEFAULT true,
    metadata    JSONB,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT entitlement_features_tenant_lookup_key_key UNIQUE (tenant_id, lookup_key)
);

ALTER TABLE billing.entitlement_features ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.entitlement_features FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.entitlement_features
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE billing.entitlement_features IS 'Stripe-shaped first-class entitlement feature definitions (issue #245). lookup_key is the stable value carried in AuthKit JWT entitlements and host-app checks. The internal billing.entitlements window ledger remains the source of truth for active access.';

-- =============================================================================
-- PAYMENT METHODS
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.payment_methods
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.payment_methods (
    id                      UUID          PRIMARY KEY DEFAULT uuidv7(),
    processor               VARCHAR(50)   NOT NULL,
    vault_id                VARCHAR(255)  NOT NULL,
    billing_id              VARCHAR(255),
    initial_transaction_id  VARCHAR(255)  NOT NULL,
    last_four               VARCHAR(4),
    card_type               VARCHAR(50),
    expiry_date             VARCHAR(5),
    failure_reason          TEXT,
    metadata                JSONB,
    created_at              TIMESTAMPTZ   NOT NULL DEFAULT current_timestamp,
    updated_at              TIMESTAMPTZ   NOT NULL DEFAULT current_timestamp,
    tenant_id               UUID          NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id       UUID          NOT NULL,
    CONSTRAINT payment_methods_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id)
);

CREATE INDEX IF NOT EXISTS idx_payment_methods_processor      ON billing.payment_methods(processor);
CREATE INDEX IF NOT EXISTS idx_payment_methods_tenant_id      ON billing.payment_methods(tenant_id);
CREATE INDEX IF NOT EXISTS idx_payment_methods_tenant_subject ON billing.payment_methods(tenant_subject_id) WHERE tenant_subject_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_payment_methods_vault_id       ON billing.payment_methods(vault_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_methods_tenant_processor_vault ON billing.payment_methods(tenant_id, processor, vault_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_methods_tenant_subject_vault   ON billing.payment_methods(tenant_id, tenant_subject_id, vault_id);

ALTER TABLE billing.payment_methods ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.payment_methods FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.payment_methods
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.payment_methods                   IS 'Generalized payment method table supporting multiple processors.';
COMMENT ON COLUMN billing.payment_methods.processor         IS 'Payment processor type: nmi, ccbill, stripe, etc.';
COMMENT ON COLUMN billing.payment_methods.vault_id          IS 'Primary payment method identifier in the processor system';
COMMENT ON COLUMN billing.payment_methods.tenant_id         IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN billing.payment_methods.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- -----------------------------------------------------------------------------
-- billing.processor_customers — processor-side customer handles per payer
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.processor_customers (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    processor          TEXT         NOT NULL,
    customer_id        TEXT         NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    tenant_id          UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id  UUID         NOT NULL,
    CONSTRAINT processor_customers_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id)
);

CREATE INDEX IF NOT EXISTS idx_processor_customers_tenant_id      ON billing.processor_customers(tenant_id);
CREATE INDEX IF NOT EXISTS idx_processor_customers_tenant_subject ON billing.processor_customers(tenant_subject_id) WHERE tenant_subject_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_processor_customers_tenant_processor_customer ON billing.processor_customers(tenant_id, processor, customer_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_processor_customers_tenant_subject_processor  ON billing.processor_customers(tenant_id, tenant_subject_id, processor);

ALTER TABLE billing.processor_customers ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.processor_customers FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.processor_customers
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON COLUMN billing.processor_customers.tenant_id         IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN billing.processor_customers.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- =============================================================================
-- SUBSCRIPTIONS, PAYMENTS, CHECKOUT
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.subscriptions
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.subscriptions (
    id                          UUID          PRIMARY KEY DEFAULT uuidv7(),
    price_id                    UUID          REFERENCES billing.prices(id),
    product_id                  UUID          NOT NULL REFERENCES billing.products(id),
    status                      billing.subscription_status NOT NULL DEFAULT 'pending',
    processor                   TEXT          NOT NULL DEFAULT 'ccbill',
    processor_subscription_id   TEXT          NOT NULL DEFAULT '',
    user_email                  TEXT,
    payment_method_id           UUID          REFERENCES billing.payment_methods(id) ON DELETE SET NULL,
    current_period_starts_at    TIMESTAMPTZ,
    current_period_ends_at      TIMESTAMPTZ,
    started_at                  TIMESTAMPTZ   NOT NULL DEFAULT current_timestamp,
    ended_at                    TIMESTAMPTZ,
    grace_ends_at               TIMESTAMPTZ,
    scheduled_price_id          UUID          REFERENCES billing.prices(id),
    last_retry_at               TIMESTAMPTZ,
    retry_attempts              INT           DEFAULT 0,
    next_retry_at               TIMESTAMPTZ,
    cancelled_at                TIMESTAMPTZ,
    cancel_type                 TEXT,
    cancel_feedback             TEXT,
    entitlements_spec_snapshot  JSONB,
    credits_spec_snapshot       JSONB,
    gateway_response            JSONB,
    created_at                  TIMESTAMPTZ   NOT NULL DEFAULT current_timestamp,
    updated_at                  TIMESTAMPTZ   NOT NULL DEFAULT current_timestamp,
    tier_group                  VARCHAR(100),
    deletion_scheduled_at       TIMESTAMPTZ,
    tenant_id                   UUID          NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id           UUID          NOT NULL,
    CONSTRAINT subscriptions_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id),
    CONSTRAINT chk_cancelled_has_timestamp     CHECK ((status <> 'cancelled'::billing.subscription_status) OR (cancelled_at IS NOT NULL)),
    CONSTRAINT chk_cancelled_has_type          CHECK ((status <> 'cancelled'::billing.subscription_status) OR (cancel_type IS NOT NULL)),
    CONSTRAINT chk_cancelled_no_retry_schedule CHECK ((status <> 'cancelled'::billing.subscription_status) OR ((next_retry_at IS NULL) AND (grace_ends_at IS NULL))),
    CONSTRAINT chk_ended_not_before_cancelled  CHECK ((ended_at IS NULL) OR (cancelled_at IS NULL) OR (ended_at >= cancelled_at)),
    CONSTRAINT chk_past_due_has_period_end      CHECK ((status <> 'past_due'::billing.subscription_status) OR (current_period_ends_at IS NOT NULL)),
    CONSTRAINT chk_valid_period                 CHECK ((current_period_starts_at IS NULL) OR (current_period_ends_at IS NULL) OR (current_period_starts_at < current_period_ends_at))
);

CREATE INDEX IF NOT EXISTS idx_subscriptions_due_dunning            ON billing.subscriptions(next_retry_at, processor) WHERE ((status = 'past_due'::billing.subscription_status) AND (next_retry_at IS NOT NULL));
CREATE INDEX IF NOT EXISTS idx_subscriptions_grace_ends_at          ON billing.subscriptions(grace_ends_at) WHERE grace_ends_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_subscriptions_next_retry_at          ON billing.subscriptions(next_retry_at) WHERE next_retry_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_subscriptions_payment_method_id      ON billing.subscriptions(payment_method_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_price_id               ON billing.subscriptions(price_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_processor              ON billing.subscriptions(processor);
CREATE INDEX IF NOT EXISTS idx_subscriptions_processor_subscription ON billing.subscriptions(processor, processor_subscription_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_product_id             ON billing.subscriptions(product_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_status                 ON billing.subscriptions(status);
CREATE INDEX IF NOT EXISTS idx_subscriptions_tenant_id              ON billing.subscriptions(tenant_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_tenant_subject         ON billing.subscriptions(tenant_subject_id) WHERE tenant_subject_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriptions_tenant_processor_subscription_id ON billing.subscriptions(tenant_id, processor, processor_subscription_id) WHERE processor_subscription_id <> ''::text;
CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriptions_tenant_subject_product_lifecycle ON billing.subscriptions(tenant_id, tenant_subject_id, product_id) WHERE (status = ANY (ARRAY['active'::billing.subscription_status, 'pending'::billing.subscription_status, 'past_due'::billing.subscription_status]));
CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriptions_tenant_subject_tier_group_active ON billing.subscriptions(tenant_subject_id, tier_group) WHERE ((status = ANY (ARRAY['active'::billing.subscription_status, 'pending'::billing.subscription_status])) AND (tier_group IS NOT NULL));

ALTER TABLE billing.subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.subscriptions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.subscriptions
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.subscriptions                    IS 'Core subscription records tracking user billing relationships';
COMMENT ON COLUMN billing.subscriptions.product_id         IS 'Denormalized product ID for efficient user+product lookups without joining prices';
COMMENT ON COLUMN billing.subscriptions.scheduled_price_id IS 'Price ID for scheduled tier change (downgrade). Applied at end of current billing period during renewal.';
COMMENT ON COLUMN billing.subscriptions.tier_group         IS 'Denormalized from billing.products.tier_group (kept in sync by trigger trg_subscriptions_set_tier_group). Backs uq_subscriptions_user_tier_group_active, which enforces one active/pending subscription per (user, tier group).';
COMMENT ON COLUMN billing.subscriptions.tenant_id          IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN billing.subscriptions.tenant_subject_id  IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- tier_group is denormalized from the product and kept in sync on insert / product change.
CREATE OR REPLACE FUNCTION billing.subscriptions_set_tier_group() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    SELECT prod.tier_group INTO NEW.tier_group
    FROM billing.products AS prod
    WHERE prod.id = NEW.product_id;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_subscriptions_set_tier_group
    BEFORE INSERT OR UPDATE OF product_id ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_set_tier_group();

-- -----------------------------------------------------------------------------
-- billing.payments — all payment transactions
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.payments (
    id                          UUID          PRIMARY KEY DEFAULT uuidv7(),
    price_id                    UUID          NOT NULL REFERENCES billing.prices(id),
    processor                   billing.processor_type NOT NULL,
    transaction_id              TEXT          NOT NULL,
    amount                      BIGINT        NOT NULL,
    list_amount                 BIGINT        NOT NULL,
    currency                    TEXT          NOT NULL DEFAULT 'usd',
    status                      billing.purchase_status NOT NULL DEFAULT 'completed',
    subscription_id             UUID          REFERENCES billing.subscriptions(id) ON DELETE SET NULL,
    refunded_payment_id         UUID          REFERENCES billing.payments(id),
    discount_code               TEXT,
    discount_reason             TEXT,
    discount_metadata           JSONB,
    entitlements_spec_snapshot  JSONB,
    credits_spec_snapshot       JSONB,
    metadata                    JSONB,
    purchased_at                TIMESTAMPTZ   NOT NULL DEFAULT current_timestamp,
    created_at                  TIMESTAMPTZ   NOT NULL DEFAULT current_timestamp,
    card_brand                  TEXT,
    card_last4                  TEXT,
    tenant_id                   UUID          NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id           UUID          NOT NULL,
    CONSTRAINT payments_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id),
    CONSTRAINT chk_payment_not_future     CHECK (purchased_at <= (now() + '00:05:00'::interval))
);

CREATE INDEX IF NOT EXISTS idx_payments_price_id            ON billing.payments(price_id);
CREATE INDEX IF NOT EXISTS idx_payments_processor           ON billing.payments(processor);
CREATE INDEX IF NOT EXISTS idx_payments_purchased_at        ON billing.payments(purchased_at);
CREATE INDEX IF NOT EXISTS idx_payments_refunded_payment_id ON billing.payments(refunded_payment_id);
CREATE INDEX IF NOT EXISTS idx_payments_subscription_id     ON billing.payments(subscription_id);
CREATE INDEX IF NOT EXISTS idx_payments_tenant_id           ON billing.payments(tenant_id);
CREATE INDEX IF NOT EXISTS idx_payments_tenant_subject      ON billing.payments(tenant_subject_id) WHERE tenant_subject_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_payments_tenant_processor_transaction ON billing.payments(tenant_id, processor, transaction_id);

ALTER TABLE billing.payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.payments FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.payments
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.payments                   IS 'Records of all payment transactions (formerly purchases table)';
COMMENT ON COLUMN billing.payments.subscription_id   IS 'Links a payment to the subscription that generated it (nullable for one-off payments)';
COMMENT ON COLUMN billing.payments.tenant_id         IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN billing.payments.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- -----------------------------------------------------------------------------
-- billing.checkout_sessions
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.checkout_sessions (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    price_id           UUID         NOT NULL REFERENCES billing.prices(id),
    mode               TEXT         NOT NULL CHECK (mode IN ('one_off','subscription')),
    processor          TEXT         NOT NULL,
    status             TEXT         NOT NULL,
    amount             BIGINT       NOT NULL,
    currency           TEXT         NOT NULL DEFAULT 'usd',
    expires_at         TIMESTAMPTZ,
    reference          TEXT,
    transaction_id     TEXT,
    payment_id         UUID         REFERENCES billing.payments(id),
    subscription_id    UUID         REFERENCES billing.subscriptions(id),
    processor_fields   JSONB,
    processor_state    JSONB,
    metadata           JSONB,
    idempotency_key    TEXT,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    tenant_id          UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id  UUID         NOT NULL,
    CONSTRAINT checkout_sessions_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id)
);

CREATE INDEX IF NOT EXISTS checkout_sessions_expires_at_idx     ON billing.checkout_sessions(expires_at);
CREATE UNIQUE INDEX IF NOT EXISTS checkout_sessions_processor_reference_idx      ON billing.checkout_sessions(processor, reference) WHERE reference IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS checkout_sessions_processor_transaction_id_idx ON billing.checkout_sessions(processor, transaction_id) WHERE transaction_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_checkout_sessions_tenant_id      ON billing.checkout_sessions(tenant_id);
CREATE INDEX IF NOT EXISTS idx_checkout_sessions_tenant_subject ON billing.checkout_sessions(tenant_subject_id) WHERE tenant_subject_id IS NOT NULL;

ALTER TABLE billing.checkout_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.checkout_sessions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.checkout_sessions
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON COLUMN billing.checkout_sessions.tenant_id         IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN billing.checkout_sessions.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- -----------------------------------------------------------------------------
-- billing.admin_grants — admin-initiated product grants
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.admin_grants (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    price_id           UUID         REFERENCES billing.prices(id),
    granted_by         TEXT         NOT NULL,
    reason             TEXT         NOT NULL,
    payment_id         UUID         REFERENCES billing.payments(id),
    duration_days      INT,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    tenant_id          UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id  UUID         NOT NULL,
    CONSTRAINT admin_grants_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id)
);

CREATE INDEX IF NOT EXISTS idx_admin_grants_granted_by     ON billing.admin_grants(granted_by);
CREATE INDEX IF NOT EXISTS idx_admin_grants_payment_id     ON billing.admin_grants(payment_id) WHERE payment_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_admin_grants_tenant_id      ON billing.admin_grants(tenant_id);
CREATE INDEX IF NOT EXISTS idx_admin_grants_tenant_subject ON billing.admin_grants(tenant_subject_id) WHERE tenant_subject_id IS NOT NULL;

ALTER TABLE billing.admin_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.admin_grants FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.admin_grants
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.admin_grants                   IS 'Records admin-initiated product grants (comps, contest winners, manual payments, partnerships)';
COMMENT ON COLUMN billing.admin_grants.price_id          IS 'Price/Product being granted - entitlements derived from Product.EntitlementsSpec';
COMMENT ON COLUMN billing.admin_grants.granted_by        IS 'Admin user ID who made the grant';
COMMENT ON COLUMN billing.admin_grants.reason            IS 'Reason for grant: comp, contest_winner, refund_compensation, partnership, manual_payment, etc.';
COMMENT ON COLUMN billing.admin_grants.payment_id        IS 'Optional link to Payment record if money was received';
COMMENT ON COLUMN billing.admin_grants.duration_days     IS 'Override entitlement duration (NULL=use Product spec, 0=indefinite, N=N days)';
COMMENT ON COLUMN billing.admin_grants.tenant_id         IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN billing.admin_grants.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- =============================================================================
-- CREDIT LEDGER
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.credit_transactions — append-only credit ledger
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.credit_transactions (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    invoker_id         TEXT         NOT NULL,
    credit_type_id     UUID         NOT NULL REFERENCES billing.credit_types(id),
    amount             BIGINT       NOT NULL,
    balance_after      BIGINT,
    transaction_type   TEXT         NOT NULL,
    source             TEXT         NOT NULL,
    source_id          TEXT,
    expires_at         TIMESTAMPTZ,
    description        TEXT,
    status             TEXT         NOT NULL DEFAULT 'posted',
    authorized_amount  BIGINT,
    captured_amount    BIGINT,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    tenant_id          UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id  UUID         NOT NULL,
    CONSTRAINT credit_transactions_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id)
);

CREATE INDEX IF NOT EXISTS idx_credit_holds_active_expires          ON billing.credit_transactions(expires_at) WHERE ((transaction_type = 'hold'::text) AND (status = 'active'::text));
CREATE INDEX IF NOT EXISTS idx_credit_transactions_payer           ON billing.credit_transactions(tenant_id, tenant_subject_id, credit_type_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_credit_transactions_payer_invoker   ON billing.credit_transactions(tenant_id, tenant_subject_id, credit_type_id, invoker_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_credit_transactions_tenant_id       ON billing.credit_transactions(tenant_id);
CREATE INDEX IF NOT EXISTS idx_credit_transactions_tenant_invoker  ON billing.credit_transactions(tenant_id, invoker_id, credit_type_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_credit_transactions_user_created    ON billing.credit_transactions(invoker_id, credit_type_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_deposit_idem_payer    ON billing.credit_transactions(tenant_id, tenant_subject_id, credit_type_id, source, source_id) WHERE ((transaction_type = 'deposit'::text) AND (source_id IS NOT NULL));
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_hold_idem_payer       ON billing.credit_transactions(tenant_id, tenant_subject_id, credit_type_id, source, source_id) WHERE (transaction_type = 'hold'::text);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_withdrawal_idem_payer ON billing.credit_transactions(tenant_id, tenant_subject_id, credit_type_id, source, source_id) WHERE ((transaction_type = 'withdrawal'::text) AND (source_id IS NOT NULL));

ALTER TABLE billing.credit_transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.credit_transactions FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.credit_transactions
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON COLUMN billing.credit_transactions.invoker_id        IS 'Principal that invoked the billable operation.';
COMMENT ON COLUMN billing.credit_transactions.tenant_id         IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN billing.credit_transactions.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- -----------------------------------------------------------------------------
-- billing.credit_blocks — credit lots (FIFO)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.credit_blocks (
    id                    UUID         PRIMARY KEY DEFAULT uuidv7(),
    invoker_id            TEXT         NOT NULL,
    credit_type_id        UUID         NOT NULL REFERENCES billing.credit_types(id),
    original_amount       BIGINT       NOT NULL,
    remaining_amount      BIGINT       NOT NULL,
    expires_at            TIMESTAMPTZ,
    source_transaction_id UUID         REFERENCES billing.credit_transactions(id),
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    tenant_id             UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id     UUID         NOT NULL,
    CONSTRAINT credit_blocks_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id)
);

CREATE INDEX IF NOT EXISTS idx_credit_blocks_payer                ON billing.credit_blocks(tenant_id, tenant_subject_id, credit_type_id, expires_at);
CREATE INDEX IF NOT EXISTS idx_credit_blocks_tenant_id            ON billing.credit_blocks(tenant_id);
CREATE INDEX IF NOT EXISTS idx_credit_blocks_tenant_invoker       ON billing.credit_blocks(tenant_id, invoker_id, credit_type_id, expires_at);
CREATE INDEX IF NOT EXISTS idx_credit_blocks_user_expires         ON billing.credit_blocks(invoker_id, credit_type_id, expires_at);
CREATE INDEX IF NOT EXISTS idx_credit_blocks_user_expires_created ON billing.credit_blocks(invoker_id, credit_type_id, expires_at, created_at);

ALTER TABLE billing.credit_blocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.credit_blocks FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.credit_blocks
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON COLUMN billing.credit_blocks.invoker_id        IS 'Principal that caused this credit block to be created.';
COMMENT ON COLUMN billing.credit_blocks.tenant_id         IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN billing.credit_blocks.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- -----------------------------------------------------------------------------
-- billing.user_credit_balances — materialized per-payer balance
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.user_credit_balances (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    invoker_id         TEXT         NOT NULL,
    credit_type_id     UUID         NOT NULL REFERENCES billing.credit_types(id),
    balance            BIGINT       NOT NULL DEFAULT 0,
    held_balance       BIGINT       NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    tenant_id          UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id  UUID         NOT NULL,
    CONSTRAINT user_credit_balances_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id)
);

CREATE INDEX IF NOT EXISTS idx_user_credit_balances_tenant_id      ON billing.user_credit_balances(tenant_id);
CREATE INDEX IF NOT EXISTS idx_user_credit_balances_tenant_invoker ON billing.user_credit_balances(tenant_id, invoker_id, credit_type_id);
CREATE INDEX IF NOT EXISTS idx_user_credit_balances_user           ON billing.user_credit_balances(invoker_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_user_credit_balances_payer_type ON billing.user_credit_balances(tenant_id, tenant_subject_id, credit_type_id);

ALTER TABLE billing.user_credit_balances ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.user_credit_balances FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.user_credit_balances
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON COLUMN billing.user_credit_balances.invoker_id        IS 'Principal that caused the balance row to be created or updated.';
COMMENT ON COLUMN billing.user_credit_balances.tenant_id         IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN billing.user_credit_balances.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- -----------------------------------------------------------------------------
-- billing.credit_account_settings — per-payer spend policy + money-in config
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.credit_account_settings (
    id                            UUID         PRIMARY KEY,
    tenant_id                     UUID         NOT NULL,
    tenant_subject_id             UUID         NOT NULL,
    credit_type_id                UUID         NOT NULL REFERENCES billing.credit_types(id) ON DELETE CASCADE,
    billing_mode                  TEXT         NOT NULL DEFAULT 'prepaid',
    max_spend_per_day_cents       BIGINT,
    max_spend_per_month_cents     BIGINT,
    max_outstanding_owed_cents    BIGINT,
    low_balance_threshold_cents   BIGINT,
    auto_topup_enabled            BOOLEAN      NOT NULL DEFAULT false,
    auto_topup_amount_cents       BIGINT,
    auto_topup_payment_method_id  UUID,
    default_credit_expiry_days    INT,
    hard_stop_on_breach           BOOLEAN      NOT NULL DEFAULT true,
    alert_threshold_pct           INT          NOT NULL DEFAULT 80,
    outstanding_owed_cents        BIGINT       NOT NULL DEFAULT 0,
    last_alert_at                 TIMESTAMPTZ,
    last_topup_at                 TIMESTAMPTZ,
    created_at                    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at                    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    verified_payment_method       BOOLEAN      NOT NULL DEFAULT false,
    verified_at                   TIMESTAMPTZ,
    suspended_at                  TIMESTAMPTZ,
    suspend_reason                TEXT,
    tier                          TEXT,
    CONSTRAINT credit_account_settings_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id),
    CONSTRAINT credit_account_settings_alert_pct_chk     CHECK ((alert_threshold_pct >= 0) AND (alert_threshold_pct <= 100)),
    CONSTRAINT credit_account_settings_billing_mode_chk  CHECK (billing_mode IN ('prepaid','arrears')),
    CONSTRAINT credit_account_settings_owed_nonneg_chk   CHECK (outstanding_owed_cents >= 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_credit_account_settings_payer_type ON billing.credit_account_settings(tenant_id, tenant_subject_id, credit_type_id);

ALTER TABLE billing.credit_account_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.credit_account_settings FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.credit_account_settings
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.credit_account_settings                         IS 'Per-(tenant, tenant subject, credit_type) spend policy + money-in config (issue #237). Tensorhub SETS these; OpenRails STORES + ENFORCES them.';
COMMENT ON COLUMN billing.credit_account_settings.tenant_subject_id       IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';
COMMENT ON COLUMN billing.credit_account_settings.verified_payment_method IS 'True once the account has a verified payment method (set after a successful $1 auth-and-void verification charge — issue #299). The charge itself is a separate slice.';
COMMENT ON COLUMN billing.credit_account_settings.suspended_at            IS 'When set, the account is suspended (issue #299). Admission-deny-on-suspended wiring is a separate slice.';

-- -----------------------------------------------------------------------------
-- billing.credit_spend_limits — optional per-invoker spend caps under an owner
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.credit_spend_limits (
    id                         UUID         PRIMARY KEY,
    tenant_id                  UUID         NOT NULL,
    tenant_subject_id          UUID         NOT NULL,
    credit_type_id             UUID         NOT NULL REFERENCES billing.credit_types(id) ON DELETE CASCADE,
    invoker_id                 TEXT         NOT NULL,
    max_spend_per_day_cents    BIGINT,
    max_spend_per_month_cents  BIGINT,
    created_at                 TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT credit_spend_limits_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_credit_spend_limits_payer_invoker ON billing.credit_spend_limits(tenant_id, tenant_subject_id, credit_type_id, invoker_id);

ALTER TABLE billing.credit_spend_limits ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.credit_spend_limits FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.credit_spend_limits
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.credit_spend_limits                   IS 'Optional per-invoker spend caps under an owner (issue #237/#246). invoker matches credit_transactions.invoker_id (the actor).';
COMMENT ON COLUMN billing.credit_spend_limits.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';
COMMENT ON COLUMN billing.credit_spend_limits.invoker_id        IS 'Principal whose spend is capped by this row.';

-- =============================================================================
-- ENTITLEMENTS (window ledger)
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.entitlements
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.entitlements (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    entitlement        TEXT         NOT NULL,
    start_at           TIMESTAMPTZ  NOT NULL,
    end_at             TIMESTAMPTZ,
    source_id          UUID         NOT NULL,
    source_type        TEXT         NOT NULL,
    revoked_at         TIMESTAMPTZ,
    revoke_reason      TEXT,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    deleted_at         TIMESTAMPTZ,
    period             tstzrange    GENERATED ALWAYS AS (tstzrange(start_at, COALESCE(end_at, 'infinity'::timestamp with time zone), '[)'::text)) STORED,
    tenant_id          UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id  UUID         NOT NULL,
    CONSTRAINT entitlements_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id),
    CONSTRAINT chk_entitlements_source_type CHECK (source_type IN ('subscription','one_off','admin','grace')),
    CONSTRAINT chk_revoke_fields_together   CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL)),
    CONSTRAINT chk_valid_time_window        CHECK ((end_at IS NULL) OR (start_at < end_at)),
    CONSTRAINT entitlements_tenant_subject_no_overlap EXCLUDE USING gist (tenant_id WITH =, tenant_subject_id WITH =, entitlement WITH =, period WITH &&) WHERE ((tenant_subject_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_entitlements_grace_by_subscription_live    ON billing.entitlements(source_id, entitlement, start_at, end_at) WHERE ((source_type = 'grace'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX IF NOT EXISTS idx_entitlements_live_by_id                    ON billing.entitlements(id) WHERE ((revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX IF NOT EXISTS idx_entitlements_one_off_source_live           ON billing.entitlements(source_id, entitlement) WHERE ((source_type = 'one_off'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX IF NOT EXISTS idx_entitlements_source                        ON billing.entitlements(source_type, source_id) WHERE source_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_entitlements_subscription_source_live      ON billing.entitlements(source_id, entitlement, end_at) WHERE ((source_type = 'subscription'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX IF NOT EXISTS idx_entitlements_tenant_id                     ON billing.entitlements(tenant_id);
CREATE INDEX IF NOT EXISTS idx_entitlements_tenant_subject_active_window  ON billing.entitlements(tenant_id, tenant_subject_id, entitlement, start_at, end_at) WHERE ((tenant_subject_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE UNIQUE INDEX IF NOT EXISTS uq_entitlements_tenant_subject_active   ON billing.entitlements(tenant_id, tenant_subject_id, entitlement) WHERE ((tenant_subject_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL) AND (end_at IS NULL));

ALTER TABLE billing.entitlements ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.entitlements FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.entitlements
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON COLUMN billing.entitlements.tenant_id         IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN billing.entitlements.tenant_subject_id IS 'OpenRails payable tenant subject for this entitlement window.';

-- -----------------------------------------------------------------------------
-- billing.product_access_grants — durable product ownership/access (#250)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.product_access_grants (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id          UUID         NOT NULL,
    product_id         UUID         NOT NULL REFERENCES billing.products(id),
    source_type        TEXT         NOT NULL,
    source_id          TEXT         NOT NULL DEFAULT '',
    payment_id         UUID,
    status             TEXT         NOT NULL DEFAULT 'active',
    starts_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    ends_at            TIMESTAMPTZ,
    revoked_at         TIMESTAMPTZ,
    revoke_reason      TEXT,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    tenant_subject_id  UUID         NOT NULL,
    CONSTRAINT product_access_grants_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id),
    CONSTRAINT product_access_grants_source_type_check CHECK (source_type IN ('purchase','subscription','admin')),
    CONSTRAINT product_access_grants_status_check      CHECK (status IN ('active','revoked'))
);

CREATE INDEX IF NOT EXISTS idx_product_access_grants_tenant_subject ON billing.product_access_grants(tenant_subject_id) WHERE tenant_subject_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS ix_product_access_grants_payment         ON billing.product_access_grants(payment_id) WHERE payment_id IS NOT NULL;

ALTER TABLE billing.product_access_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.product_access_grants FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.product_access_grants
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.product_access_grants                   IS 'Durable, application-facing product ownership/access (issue #250). Distinct from feature entitlements: answers "does this user own product X?" / "list products this user can access". A successful one-time purchase creates a grant; refunds/chargebacks/admin revocation revoke it.';
COMMENT ON COLUMN billing.product_access_grants.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- -----------------------------------------------------------------------------
-- billing.product_entitlement_features — product -> feature attachments (#245)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.product_entitlement_features (
    id                     UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id              UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    product_id             UUID         NOT NULL REFERENCES billing.products(id) ON DELETE CASCADE,
    entitlement_feature_id UUID         NOT NULL REFERENCES billing.entitlement_features(id) ON DELETE CASCADE,
    duration_days          INT,
    metadata               JSONB,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT product_entitlement_features_unique UNIQUE (tenant_id, product_id, entitlement_feature_id)
);

CREATE INDEX IF NOT EXISTS idx_product_entitlement_features_feature ON billing.product_entitlement_features(tenant_id, entitlement_feature_id);
CREATE INDEX IF NOT EXISTS idx_product_entitlement_features_product ON billing.product_entitlement_features(tenant_id, product_id);

ALTER TABLE billing.product_entitlement_features ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.product_entitlement_features FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.product_entitlement_features
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE billing.product_entitlement_features IS 'Stripe-shaped product_feature attachments (issue #245): which entitlement features a product grants when purchased. duration_days null = indefinite.';

-- =============================================================================
-- DUNNING, NOTIFICATIONS, SOLANA
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.manual_rebill_attempts
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.manual_rebill_attempts (
    id               UUID         PRIMARY KEY DEFAULT uuidv7(),
    subscription_id  UUID         NOT NULL REFERENCES billing.subscriptions(id),
    period_end       TIMESTAMPTZ  NOT NULL,
    processor        TEXT         NOT NULL,
    order_reference  TEXT         NOT NULL,
    status           TEXT         NOT NULL CHECK (status IN ('pending','succeeded','failed','unknown')),
    transaction_id   TEXT,
    failure_reason   TEXT,
    claimed_until    TIMESTAMPTZ,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    tenant_id        UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    CONSTRAINT uniq_manual_rebill_processor_order      UNIQUE (processor, order_reference),
    CONSTRAINT uniq_manual_rebill_subscription_period  UNIQUE (subscription_id, period_end)
);

CREATE INDEX IF NOT EXISTS idx_manual_rebill_attempts_status_claimed ON billing.manual_rebill_attempts(status, claimed_until);
CREATE INDEX IF NOT EXISTS idx_manual_rebill_attempts_tenant_id      ON billing.manual_rebill_attempts(tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_manual_rebill_processor_transaction ON billing.manual_rebill_attempts(processor, transaction_id) WHERE transaction_id IS NOT NULL;

ALTER TABLE billing.manual_rebill_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.manual_rebill_attempts FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.manual_rebill_attempts
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON COLUMN billing.manual_rebill_attempts.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';

-- -----------------------------------------------------------------------------
-- billing.notification_queue
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.notification_queue (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    event_type         TEXT         NOT NULL,
    data               JSONB        NOT NULL,
    seen               BOOLEAN      NOT NULL DEFAULT false,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    tenant_id          UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id  UUID         NOT NULL,
    CONSTRAINT notification_queue_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id)
);

CREATE INDEX IF NOT EXISTS idx_notification_queue_created_at    ON billing.notification_queue(created_at);
CREATE INDEX IF NOT EXISTS idx_notification_queue_event_type    ON billing.notification_queue(event_type);
CREATE INDEX IF NOT EXISTS idx_notification_queue_seen          ON billing.notification_queue(seen);
CREATE INDEX IF NOT EXISTS idx_notification_queue_tenant_id     ON billing.notification_queue(tenant_id);
CREATE INDEX IF NOT EXISTS idx_notification_queue_tenant_subject ON billing.notification_queue(tenant_subject_id) WHERE tenant_subject_id IS NOT NULL;

ALTER TABLE billing.notification_queue ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.notification_queue FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.notification_queue
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.notification_queue                   IS 'Queue for user notifications related to billing and subscriptions';
COMMENT ON COLUMN billing.notification_queue.tenant_id         IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
COMMENT ON COLUMN billing.notification_queue.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- -----------------------------------------------------------------------------
-- billing.solana_subscriptions — on-chain subscription pull state (#261)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.solana_subscriptions (
    id                           UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id                    UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    subscription_id              UUID         NOT NULL REFERENCES billing.subscriptions(id) ON DELETE CASCADE,
    subscriber_wallet            TEXT         NOT NULL,
    authority_pda                TEXT         NOT NULL,
    subscription_pda             TEXT         NOT NULL,
    plan_pda                     TEXT         NOT NULL,
    merchant_address             TEXT         NOT NULL,
    mint                         TEXT         NOT NULL,
    plan_created_at_fingerprint  BIGINT       NOT NULL,
    last_pulled_period_start     TIMESTAMPTZ,
    last_signature               TEXT,
    next_pull_at                 TIMESTAMPTZ  NOT NULL,
    status                       TEXT         NOT NULL DEFAULT 'active',
    created_at                   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at                   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT solana_subscriptions_subscription_pda_key UNIQUE (subscription_pda)
);

CREATE INDEX IF NOT EXISTS idx_solana_subscriptions_due             ON billing.solana_subscriptions(tenant_id, next_pull_at) WHERE (status = 'active'::text);
CREATE INDEX IF NOT EXISTS idx_solana_subscriptions_subscription_id ON billing.solana_subscriptions(subscription_id);

-- =============================================================================
-- USAGE, INVOICING, ADMISSION (#289 / #298 / #303 / #304)
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.usage_events — append-only metered usage
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.usage_events (
    id                    UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id             UUID         NOT NULL,
    tenant_subject_id     UUID         NOT NULL,
    invoker_id            TEXT         NOT NULL,
    credit_type_id        UUID         NOT NULL,
    event_type            TEXT         NOT NULL,
    dimensions            JSONB        NOT NULL DEFAULT '{}'::jsonb,
    amount                BIGINT       NOT NULL,
    source                TEXT         NOT NULL,
    source_id             TEXT         NOT NULL,
    credit_transaction_id UUID,
    metadata              JSONB,
    occurred_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT usage_events_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id),
    CONSTRAINT usage_events_amount_check      CHECK (amount >= 0)
);

CREATE INDEX IF NOT EXISTS idx_usage_events_tenant_subject_time ON billing.usage_events(tenant_subject_id, occurred_at);
CREATE INDEX IF NOT EXISTS ix_usage_events_payer_time          ON billing.usage_events(tenant_id, tenant_subject_id, occurred_at);
CREATE INDEX IF NOT EXISTS ix_usage_events_payer_type_time     ON billing.usage_events(tenant_id, tenant_subject_id, event_type, occurred_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_usage_events_idem          ON billing.usage_events(tenant_id, tenant_subject_id, event_type, source, source_id);

ALTER TABLE billing.usage_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.usage_events FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.usage_events
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.usage_events                   IS 'Append-only multi-dimensional metered usage (issue #289). Source of truth for usage reporting + #303 invoice line items. Host-priced (amount sent by the host); event + ledger debit commit in one tx. The hot admission path (#298) never reads this table.';
COMMENT ON COLUMN billing.usage_events.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';
COMMENT ON COLUMN billing.usage_events.invoker_id        IS 'Principal that invoked this metered usage event.';

-- -----------------------------------------------------------------------------
-- billing.invoices — monthly itemized statements (#303)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.invoices (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id          UUID         NOT NULL,
    tenant_subject_id  UUID         NOT NULL,
    credit_type_id     UUID         NOT NULL,
    currency           TEXT         NOT NULL DEFAULT '',
    period_from        TIMESTAMPTZ  NOT NULL,
    period_to          TIMESTAMPTZ  NOT NULL,
    usage_total        BIGINT       NOT NULL DEFAULT 0,
    deposits_total     BIGINT       NOT NULL DEFAULT 0,
    owed_accrued       BIGINT       NOT NULL DEFAULT 0,
    owed_paid          BIGINT       NOT NULL DEFAULT 0,
    closing_balance    BIGINT       NOT NULL DEFAULT 0,
    line_items         JSONB        NOT NULL DEFAULT '[]'::jsonb,
    money_movements    JSONB        NOT NULL DEFAULT '{}'::jsonb,
    status             TEXT         NOT NULL DEFAULT 'draft',
    finalized_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT invoices_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id),
    CONSTRAINT invoices_status_check      CHECK (status IN ('draft','finalized','voided'))
);

CREATE INDEX IF NOT EXISTS idx_invoices_tenant_subject ON billing.invoices(tenant_subject_id, period_from DESC);
CREATE INDEX IF NOT EXISTS ix_invoices_payer           ON billing.invoices(tenant_id, tenant_subject_id, period_from DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_invoices_period    ON billing.invoices(tenant_id, tenant_subject_id, credit_type_id, period_from, period_to);

ALTER TABLE billing.invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.invoices FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.invoices
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.invoices                   IS 'Monthly itemized statements (issue #303). Line items rolled up from billing.usage_events; money movements from the credit ledger; snapshotted at finalize. Prepaid = receipt, arrears = statement the #301 sweep settles.';
COMMENT ON COLUMN billing.invoices.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- -----------------------------------------------------------------------------
-- billing.tier_policies — per-owner tier throughput policies (#298)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tier_policies (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id          UUID         NOT NULL,
    tenant_subject_id  UUID         NOT NULL,
    tier               TEXT         NOT NULL,
    policy             JSONB        NOT NULL DEFAULT '{}'::jsonb,
    policy_version     BIGINT       NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT tier_policies_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_tier_policies ON billing.tier_policies(tenant_id, tenant_subject_id, tier);

ALTER TABLE billing.tier_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.tier_policies FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.tier_policies
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.tier_policies                   IS 'Per-owner tier throughput policies for the admission check (issue #298). MONEY caps stay in credit_account_settings; rolling money budgets are #304.';
COMMENT ON COLUMN billing.tier_policies.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- -----------------------------------------------------------------------------
-- billing.budget_reservations — rolling-window money-budget reservations (#304)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.budget_reservations (
    id                   UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id            UUID         NOT NULL,
    tenant_subject_id    UUID         NOT NULL,
    invoker_id           TEXT         NOT NULL,
    amount_millicents    BIGINT       NOT NULL,
    captured_millicents  BIGINT       NOT NULL DEFAULT 0,
    status               TEXT         NOT NULL DEFAULT 'active',
    source               TEXT         NOT NULL,
    source_id            TEXT         NOT NULL,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at           TIMESTAMPTZ,
    CONSTRAINT budget_reservations_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id),
    CONSTRAINT budget_reservations_status_check      CHECK (status IN ('active','captured','released'))
);

CREATE INDEX IF NOT EXISTS ix_budget_reservations_window ON billing.budget_reservations(tenant_id, tenant_subject_id, invoker_id, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_budget_reservations_idem ON billing.budget_reservations(tenant_id, tenant_subject_id, invoker_id, source, source_id);

ALTER TABLE billing.budget_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.budget_reservations FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.budget_reservations
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.budget_reservations                   IS 'Rolling-window money-budget reservations (issue #304). One row per in-flight/settled charge against an actor''s passed-in windows; used/reserved/remaining are windowed SUM() over created_at. Idempotent on (tenant, owner, actor, source, source_id).';
COMMENT ON COLUMN billing.budget_reservations.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';
COMMENT ON COLUMN billing.budget_reservations.invoker_id        IS 'Principal whose rolling money-budget windows are capped.';

-- -----------------------------------------------------------------------------
-- billing.payment_blocklist — known-bad payment identifiers (#300)
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.payment_blocklist (
    id                 UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id          UUID         NOT NULL,
    tenant_subject_id  UUID,
    kind               TEXT         NOT NULL CHECK (kind IN ('card_fingerprint','processor_customer','email','ip')),
    value              TEXT         NOT NULL,
    reason             TEXT,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT payment_blocklist_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_blocklist ON billing.payment_blocklist(tenant_id, kind, value);

ALTER TABLE billing.payment_blocklist ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.payment_blocklist FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.payment_blocklist
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.payment_blocklist                   IS 'Tenant-scoped blocklist of known-bad payment identifiers (issue #300). tenant_subject_id NULL = tenant-wide block; set = subject-scoped. Checkout/admission deny wiring is a separate slice.';
COMMENT ON COLUMN billing.payment_blocklist.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';

-- =============================================================================
-- WALLETS & SELF-SERVICE FUNDING
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.linked_wallets — verified user wallet links
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.linked_wallets (
    id                     UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id              UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id      UUID         NOT NULL REFERENCES billing.tenant_subjects(id) ON DELETE CASCADE,
    chain                  TEXT         NOT NULL,
    address                TEXT         NOT NULL,
    verification_provider  TEXT         NOT NULL,
    verified_at            TIMESTAMPTZ  NOT NULL,
    display_name           TEXT,
    metadata               JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT linked_wallets_chain_address_nonempty CHECK ((btrim(chain) <> ''::text) AND (btrim(address) <> ''::text)),
    CONSTRAINT linked_wallets_unique_chain_address UNIQUE (tenant_id, chain, address),
    CONSTRAINT linked_wallets_unique_subject_chain UNIQUE (tenant_id, tenant_subject_id, chain)
);

CREATE INDEX IF NOT EXISTS idx_linked_wallets_tenant_subject ON billing.linked_wallets(tenant_id, tenant_subject_id);

COMMENT ON TABLE billing.linked_wallets IS 'Verified user wallet links for browser self-service billing identity. The wallet must come from trusted delegated-token claims, not request body input.';

-- -----------------------------------------------------------------------------
-- billing.usdc_funding_sessions — external Robinhood/Coinbase USDC handoffs
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.usdc_funding_sessions (
    id                   UUID         PRIMARY KEY DEFAULT uuidv7(),
    tenant_id            UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
    tenant_subject_id    UUID         NOT NULL REFERENCES billing.tenant_subjects(id) ON DELETE CASCADE,
    checkout_session_id  UUID         REFERENCES billing.checkout_sessions(id) ON DELETE SET NULL,
    provider             TEXT         NOT NULL,
    wallet_address       TEXT         NOT NULL,
    asset                TEXT         NOT NULL,
    network              TEXT         NOT NULL,
    requested_amount     TEXT         NOT NULL,
    provider_session_id  TEXT,
    provider_url         TEXT         NOT NULL,
    status               TEXT         NOT NULL,
    return_url           TEXT,
    idempotency_key      TEXT,
    metadata             JSONB        NOT NULL DEFAULT '{}'::jsonb,
    last_checked_at      TIMESTAMPTZ,
    expires_at           TIMESTAMPTZ,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT usdc_funding_sessions_asset_valid    CHECK (asset = 'USDC'::text),
    CONSTRAINT usdc_funding_sessions_nonempty       CHECK ((btrim(wallet_address) <> ''::text) AND (btrim(network) <> ''::text) AND (btrim(requested_amount) <> ''::text) AND (btrim(provider_url) <> ''::text)),
    CONSTRAINT usdc_funding_sessions_provider_valid CHECK (provider IN ('robinhood','coinbase')),
    CONSTRAINT usdc_funding_sessions_status_valid   CHECK (status IN ('created','opened','pending_provider','pending_settlement','funded','failed','expired','cancelled'))
);

CREATE INDEX IF NOT EXISTS idx_usdc_funding_sessions_checkout         ON billing.usdc_funding_sessions(tenant_id, checkout_session_id) WHERE checkout_session_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_usdc_funding_sessions_idempotency ON billing.usdc_funding_sessions(tenant_id, tenant_subject_id, idempotency_key) WHERE ((idempotency_key IS NOT NULL) AND (btrim(idempotency_key) <> ''::text));
CREATE INDEX IF NOT EXISTS idx_usdc_funding_sessions_provider_session ON billing.usdc_funding_sessions(provider, provider_session_id) WHERE provider_session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_usdc_funding_sessions_tenant_subject   ON billing.usdc_funding_sessions(tenant_id, tenant_subject_id, created_at DESC);

COMMENT ON TABLE billing.usdc_funding_sessions IS 'External Robinhood/Coinbase handoffs that fund USDC into a user self-custody wallet before normal OpenRails wallet checkout. Return from provider is not proof of funding.';

-- =============================================================================
-- CATALOG RECONCILIATION
-- =============================================================================

-- -----------------------------------------------------------------------------
-- billing.catalog_drift_events
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.catalog_drift_events (
    id                       UUID         PRIMARY KEY DEFAULT uuidv7(),
    provider                 TEXT         NOT NULL CHECK (provider IN ('stripe','nmi')),
    kind                     TEXT         NOT NULL CHECK (kind IN ('orphan_in_stripe','missing_in_stripe','orphan_in_nmi','missing_in_nmi','field_drift')),
    openrails_resource_type  TEXT         NOT NULL CHECK (openrails_resource_type IN ('product','price')),
    openrails_resource_id    TEXT,
    external_resource_id     TEXT,
    field                    TEXT,
    openrails_value          TEXT,
    external_value           TEXT,
    detected_at              TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    resolved_at              TIMESTAMPTZ,
    tenant_id                UUID         NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid
);

CREATE INDEX IF NOT EXISTS idx_catalog_drift_events_open      ON billing.catalog_drift_events(detected_at DESC) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_catalog_drift_events_tenant_id ON billing.catalog_drift_events(tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_catalog_drift_open ON billing.catalog_drift_events(provider, kind, openrails_resource_type, COALESCE(openrails_resource_id, ''::text), COALESCE(external_resource_id, ''::text), COALESCE(field, ''::text)) WHERE resolved_at IS NULL;

ALTER TABLE billing.catalog_drift_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.catalog_drift_events FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON billing.catalog_drift_events
    USING       (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid)
    WITH CHECK  (tenant_id = (NULLIF(current_setting('app.tenant_id', true), ''))::uuid);

COMMENT ON TABLE  billing.catalog_drift_events           IS 'Alert-only drift/orphan records from the catalog reconciliation loop; resolved via per-price reconcile.';
COMMENT ON COLUMN billing.catalog_drift_events.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';
