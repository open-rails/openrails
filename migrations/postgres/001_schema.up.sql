-- =============================================================================
-- OpenRails billing schema — single consolidated Postgres schema.
--
-- Final structural state of the billing schema, hand-written in topological
-- order (a table follows every table it FKs to, so FKs are inline named
-- table-constraints). No "default merchant": merchant_id is REQUIRED on every
-- merchant-scoped table — uuid NOT NULL with NO default. Writers must stamp
-- merchant_id explicitly; the RLS merchant_isolation policy enforces it against the
-- app.merchant_id GUC. No seed rows are created here: merchants and custom credit
-- units are provisioned explicitly per merchant, not seeded.
-- =============================================================================

SET statement_timeout = '300s';
SET lock_timeout = '10s';
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

CREATE SCHEMA IF NOT EXISTS openrails;

CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'openrails_app') THEN
        CREATE ROLE openrails_app NOLOGIN NOBYPASSRLS;
    END IF;
END $$;

CREATE TYPE openrails.processor_type AS ENUM (
    'paypal',
    'solana',
    'mobius',
    'ccbill',
    'stripe',
    'admin',
    'nmi',
    'manual'
);

CREATE TYPE openrails.purchase_status AS ENUM (
    'pending',
    'completed',
    'failed',
    'refunded'
);

CREATE TYPE openrails.subscription_status AS ENUM (
    'pending',
    'active',
    'expired',
    'cancelled',
    'failed',
    'past_due'
);

CREATE FUNCTION openrails.subscriptions_set_tier_group() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    SELECT prod.tier_group INTO NEW.tier_group
    FROM openrails.products AS prod
    WHERE prod.id = NEW.product_id;
    RETURN NEW;
END;
$$;

SET default_tablespace = '';
SET default_table_access_method = heap;

-- =============================================================================
-- merchants
-- =============================================================================

CREATE TABLE openrails.merchants (
    id uuid DEFAULT uuidv7() NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    owner_org_id text,
    plan text,
    region text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    suspended_at timestamp with time zone,
    deleted_at timestamp with time zone,
    billing_tier text,
    stripe_account_id text,
    webhook_host text,
    webhook_path text,
    provisioned_at timestamp with time zone,
    CONSTRAINT merchants_pkey PRIMARY KEY (id),
    CONSTRAINT uq_merchants_slug UNIQUE (slug),
    CONSTRAINT merchants_status_check CHECK ((status = ANY (ARRAY['active'::text, 'suspended'::text, 'deleted'::text])))
);

CREATE INDEX idx_merchants_owner_org_id ON openrails.merchants USING btree (owner_org_id) WHERE (owner_org_id IS NOT NULL);
CREATE UNIQUE INDEX uq_merchants_webhook_host ON openrails.merchants USING btree (lower(webhook_host)) WHERE (webhook_host IS NOT NULL);

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchants TO openrails_app;

COMMENT ON TABLE openrails.merchants IS 'Merchant / billing-namespace directory: a dumb billing bucket (whose books a row goes on). GLOBAL (control-plane) table, not tenant-scoped. Carries ONLY billing/money-rail state, NO auth. Merchants are registered explicitly; there is no default merchant.';
COMMENT ON COLUMN openrails.merchants.slug IS 'Stable merchant slug used in merchant-scoped routes and resolution.';
COMMENT ON COLUMN openrails.merchants.owner_org_id IS 'OWNERSHIP (not identity) FK to the AuthKit org that owns/administers this merchant (#481). NULL in embedded (no AuthKit). One org owns MANY merchants; never used to resolve a merchant from a token.';
COMMENT ON COLUMN openrails.merchants.billing_tier IS 'The platform''s OWN billing tier for this merchant (eats own dogfood, issue #225). Distinct from plan (free-form hosting metadata).';
COMMENT ON COLUMN openrails.merchants.webhook_host IS 'Optional host an ingress uses to route inbound webhooks to this merchant. OpenRails verifies the signature AFTER merchant resolution (router is not the trust boundary).';

-- =============================================================================
-- customers
-- =============================================================================

CREATE TABLE openrails.customers (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    org_id text,
    issuer text,
    subject text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT customers_pkey PRIMARY KEY (id),
    CONSTRAINT customers_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id)
);

CREATE INDEX idx_customers_merchant ON openrails.customers USING btree (merchant_id);
CREATE UNIQUE INDEX uq_customers_merchant_org ON openrails.customers USING btree (merchant_id, org_id) WHERE (org_id IS NOT NULL);
CREATE UNIQUE INDEX uq_customers_merchant_issuer_subject ON openrails.customers USING btree (merchant_id, issuer, subject) WHERE ((org_id IS NULL) AND (issuer IS NOT NULL) AND (subject IS NOT NULL));

ALTER TABLE ONLY openrails.customers FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.customers ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.customers USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.customers TO openrails_app;

COMMENT ON TABLE openrails.customers IS 'OpenRails payable identity. id is the payable UUID; org-bound issuers resolve one customer per (merchant, org_id), org-less issuers resolve by (merchant, issuer, subject), and native/embedded callers may materialize the subject UUID directly as id.';
COMMENT ON COLUMN openrails.customers.org_id IS '#491 org-bound payer natural key: authkit org id. One customer per (merchant, org). NULL for org-less/native payers.';
COMMENT ON COLUMN openrails.customers.issuer IS '#491 org-less payer natural key part: validated issuer. NULL for org-bound and native payers.';
COMMENT ON COLUMN openrails.customers.subject IS '#491 org-less payer natural key part: merchant-supplied stable subject. NULL for org-bound and native payers.';

-- =============================================================================
-- merchant_deks
-- =============================================================================

CREATE TABLE openrails.merchant_deks (
    merchant_id uuid NOT NULL,
    wrapped_dek bytea NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT pk_merchant_deks PRIMARY KEY (merchant_id)
);

ALTER TABLE ONLY openrails.merchant_deks FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.merchant_deks ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.merchant_deks USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_deks TO openrails_app;

COMMENT ON TABLE openrails.merchant_deks IS 'Wrapped per-merchant Data Encryption Keys for envelope encryption-at-rest (issue #227). wrapped_dek = merchant DEK sealed with the master key (AES-256-GCM, nonce||ct||tag). Master key lives in config/env (self-hosted) or KMS (production), never in the DB. Merchant-owned and RLS protected.';
COMMENT ON COLUMN openrails.merchant_deks.wrapped_dek IS 'AES-256-GCM(master_key, merchant_dek): nonce(12) || ciphertext(32) || tag(16).';

-- =============================================================================
-- merchant_secrets
-- =============================================================================

CREATE TABLE openrails.merchant_secrets (
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    value text NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT pk_merchant_secrets PRIMARY KEY (merchant_id, name)
);

ALTER TABLE ONLY openrails.merchant_secrets FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.merchant_secrets ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.merchant_secrets USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_secrets TO openrails_app;

COMMENT ON TABLE openrails.merchant_secrets IS 'DB-backed per-merchant secret store (issue #225). Namespaced by (merchant_id, name). The Vault-backed store keeps the same addressing but holds values in Vault. Merchant-owned and RLS protected.';

-- =============================================================================
-- merchant_exports
-- =============================================================================

CREATE TABLE openrails.merchant_exports (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    status text DEFAULT 'completed'::text NOT NULL,
    location text,
    row_counts jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT merchant_exports_pkey PRIMARY KEY (id),
    CONSTRAINT merchant_exports_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'completed'::text, 'failed'::text])))
);

CREATE INDEX idx_merchant_exports_merchant ON openrails.merchant_exports USING btree (merchant_id, created_at DESC);

ALTER TABLE ONLY openrails.merchant_exports FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.merchant_exports ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.merchant_exports USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_exports TO openrails_app;

COMMENT ON TABLE openrails.merchant_exports IS 'Merchant logical-export bookkeeping (issue #225). Merchant deletion is gated on a completed export row (export-before-delete). Merchant-owned and RLS protected.';

-- =============================================================================
-- merchant_credential_audit
-- =============================================================================

CREATE TABLE openrails.merchant_credential_audit (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    action text NOT NULL,
    actor text,
    detail text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT merchant_credential_audit_pkey PRIMARY KEY (id),
    CONSTRAINT merchant_credential_audit_action_check CHECK ((action = ANY (ARRAY['put'::text, 'rotate'::text, 'delete'::text, 'test'::text])))
);

CREATE INDEX idx_merchant_credential_audit_merchant ON openrails.merchant_credential_audit USING btree (merchant_id, created_at DESC);

ALTER TABLE ONLY openrails.merchant_credential_audit FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.merchant_credential_audit ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.merchant_credential_audit USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_credential_audit TO openrails_app;

COMMENT ON TABLE openrails.merchant_credential_audit IS 'Append-only audit log of per-merchant credential put/rotate/delete/test events (issue #225). Merchant-owned and RLS protected.';

-- =============================================================================
-- products
-- =============================================================================

CREATE TABLE openrails.products (
    id uuid DEFAULT uuidv7() NOT NULL,
    slug text NOT NULL,
    display_name text NOT NULL,
    description text,
    entitlements_spec jsonb,
    credits_spec jsonb,
    tier_group character varying(100),
    tier_rank integer DEFAULT 0 NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    CONSTRAINT products_pkey PRIMARY KEY (id),
    CONSTRAINT products_merchant_slug_key UNIQUE (merchant_id, slug),
    CONSTRAINT products_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'active'::text, 'archived'::text])))
);

ALTER TABLE ONLY openrails.products FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_products_slug ON openrails.products USING btree (slug);
CREATE INDEX idx_products_status ON openrails.products USING btree (status);
CREATE INDEX idx_products_merchant_id ON openrails.products USING btree (merchant_id);
CREATE INDEX idx_products_tier_group ON openrails.products USING btree (tier_group) WHERE (tier_group IS NOT NULL);

ALTER TABLE openrails.products ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.products USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.products TO openrails_app;

COMMENT ON TABLE openrails.products IS 'Product definitions that can be purchased or subscribed to';
COMMENT ON COLUMN openrails.products.credits_spec IS 'Bundled promo credits spec (amount, expiry, cadence) for subscriptions';
COMMENT ON COLUMN openrails.products.tier_group IS 'Semantic group name for mutually-exclusive products (e.g., "premium"). Products in same group require upgrade/downgrade, not parallel ownership.';
COMMENT ON COLUMN openrails.products.tier_rank IS 'Tier ranking within group. Higher = more premium. Used to determine upgrade (higher rank) vs downgrade (lower rank) direction.';

-- =============================================================================
-- prices
-- =============================================================================

CREATE TABLE openrails.prices (
    id uuid DEFAULT uuidv7() NOT NULL,
    product_id uuid NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    billing_cycle_days integer,
    processors jsonb,
    status text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    CONSTRAINT prices_pkey PRIMARY KEY (id),
    CONSTRAINT unique_prices_product_amount_cycle UNIQUE (product_id, amount, currency, billing_cycle_days),
    CONSTRAINT prices_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'active'::text, 'archived'::text]))),
    CONSTRAINT prices_product_id_fkey FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE RESTRICT
);

ALTER TABLE ONLY openrails.prices FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_prices_processors ON openrails.prices USING gin (processors);
CREATE INDEX idx_prices_product_id ON openrails.prices USING btree (product_id);
CREATE INDEX idx_prices_status ON openrails.prices USING btree (status);
CREATE INDEX idx_prices_merchant_id ON openrails.prices USING btree (merchant_id);
CREATE UNIQUE INDEX uq_prices_id_product_merchant ON openrails.prices USING btree (id, product_id, merchant_id);

ALTER TABLE openrails.prices ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.prices USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.prices TO openrails_app;

COMMENT ON TABLE openrails.prices IS 'Pricing tiers for products with processor-specific identifiers';

-- =============================================================================
-- entitlement_features
-- =============================================================================

CREATE TABLE openrails.entitlement_features (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    lookup_key text NOT NULL,
    name text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    metadata jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT entitlement_features_pkey PRIMARY KEY (id),
    CONSTRAINT entitlement_features_merchant_lookup_key_key UNIQUE (merchant_id, lookup_key)
);

ALTER TABLE ONLY openrails.entitlement_features FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.entitlement_features ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.entitlement_features USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.entitlement_features TO openrails_app;

COMMENT ON TABLE openrails.entitlement_features IS 'Stripe-shaped first-class entitlement feature definitions (issue #245). lookup_key is the stable value carried in AuthKit JWT entitlements and host-app checks. The internal openrails.entitlements window ledger remains the source of truth for active access.';

-- =============================================================================
-- product_entitlement_features
-- =============================================================================

CREATE TABLE openrails.product_entitlement_features (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    entitlement_feature_id uuid NOT NULL,
    duration_days integer,
    metadata jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT product_entitlement_features_pkey PRIMARY KEY (id),
    CONSTRAINT product_entitlement_features_unique UNIQUE (merchant_id, product_id, entitlement_feature_id),
    CONSTRAINT product_entitlement_features_product_id_fkey FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE CASCADE,
    CONSTRAINT product_entitlement_features_entitlement_feature_id_fkey FOREIGN KEY (entitlement_feature_id) REFERENCES openrails.entitlement_features(id) ON DELETE CASCADE
);

ALTER TABLE ONLY openrails.product_entitlement_features FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_product_entitlement_features_feature ON openrails.product_entitlement_features USING btree (merchant_id, entitlement_feature_id);
CREATE INDEX idx_product_entitlement_features_product ON openrails.product_entitlement_features USING btree (merchant_id, product_id);

ALTER TABLE openrails.product_entitlement_features ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.product_entitlement_features USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.product_entitlement_features TO openrails_app;

COMMENT ON TABLE openrails.product_entitlement_features IS 'Stripe-shaped product_feature attachments (issue #245): which entitlement features a product grants when purchased. duration_days null = indefinite.';

-- =============================================================================
-- payment_methods
-- =============================================================================

CREATE TABLE openrails.payment_methods (
    id uuid DEFAULT uuidv7() NOT NULL,
    processor character varying(50) NOT NULL,
    vault_id character varying(255) NOT NULL,
    billing_id character varying(255),
    initial_transaction_id character varying(255) NOT NULL,
    last_four character varying(4),
    card_type character varying(50),
    expiry_date character varying(5),
    failure_reason text,
    metadata jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    CONSTRAINT payment_methods_pkey PRIMARY KEY (id),
    CONSTRAINT payment_methods_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.payment_methods FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_payment_methods_processor ON openrails.payment_methods USING btree (processor);
CREATE INDEX idx_payment_methods_merchant_id ON openrails.payment_methods USING btree (merchant_id);
CREATE INDEX idx_payment_methods_customer ON openrails.payment_methods USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE INDEX idx_payment_methods_vault_id ON openrails.payment_methods USING btree (vault_id);
CREATE UNIQUE INDEX uq_payment_methods_merchant_processor_vault ON openrails.payment_methods USING btree (merchant_id, processor, vault_id);
CREATE UNIQUE INDEX uq_payment_methods_customer_vault ON openrails.payment_methods USING btree (merchant_id, customer_id, vault_id);

ALTER TABLE openrails.payment_methods ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.payment_methods USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payment_methods TO openrails_app;

COMMENT ON TABLE openrails.payment_methods IS 'Generalized payment method table supporting multiple processors.';
COMMENT ON COLUMN openrails.payment_methods.processor IS 'Payment processor type: nmi, ccbill, stripe, etc.';
COMMENT ON COLUMN openrails.payment_methods.vault_id IS 'Primary payment method identifier in the processor system';

-- =============================================================================
-- subscriptions
-- =============================================================================

CREATE TABLE openrails.subscriptions (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid,
    product_id uuid NOT NULL,
    status openrails.subscription_status DEFAULT 'pending'::openrails.subscription_status NOT NULL,
    processor text DEFAULT 'ccbill'::text NOT NULL,
    processor_subscription_id text DEFAULT ''::text NOT NULL,
    user_email text,
    payment_method_id uuid,
    current_period_starts_at timestamp with time zone,
    current_period_ends_at timestamp with time zone,
    started_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    ended_at timestamp with time zone,
    grace_ends_at timestamp with time zone,
    scheduled_price_id uuid,
    last_retry_at timestamp with time zone,
    retry_attempts integer DEFAULT 0,
    next_retry_at timestamp with time zone,
    cancelled_at timestamp with time zone,
    cancel_type text,
    cancel_feedback text,
    entitlements_spec_snapshot jsonb,
    credits_spec_snapshot jsonb,
    gateway_response jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    tier_group character varying(100),
    deletion_scheduled_at timestamp with time zone,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    CONSTRAINT subscriptions_pkey PRIMARY KEY (id),
    CONSTRAINT chk_cancelled_has_timestamp CHECK (((status <> 'cancelled'::openrails.subscription_status) OR (cancelled_at IS NOT NULL))),
    CONSTRAINT chk_cancelled_has_type CHECK (((status <> 'cancelled'::openrails.subscription_status) OR (cancel_type IS NOT NULL))),
    CONSTRAINT chk_cancelled_no_retry_schedule CHECK (((status <> 'cancelled'::openrails.subscription_status) OR ((next_retry_at IS NULL) AND (grace_ends_at IS NULL)))),
    CONSTRAINT chk_ended_not_before_cancelled CHECK (((ended_at IS NULL) OR (cancelled_at IS NULL) OR (ended_at >= cancelled_at))),
    CONSTRAINT chk_past_due_has_period_end CHECK (((status <> 'past_due'::openrails.subscription_status) OR (current_period_ends_at IS NOT NULL))),
    CONSTRAINT chk_valid_period CHECK (((current_period_starts_at IS NULL) OR (current_period_ends_at IS NULL) OR (current_period_starts_at < current_period_ends_at))),
    CONSTRAINT subscriptions_payment_method_id_fkey FOREIGN KEY (payment_method_id) REFERENCES openrails.payment_methods(id) ON DELETE SET NULL,
    CONSTRAINT subscriptions_price_id_fkey FOREIGN KEY (price_id) REFERENCES openrails.prices(id),
    CONSTRAINT subscriptions_price_product_merchant_fkey FOREIGN KEY (price_id, product_id, merchant_id) REFERENCES openrails.prices(id, product_id, merchant_id),
    CONSTRAINT subscriptions_product_id_fkey FOREIGN KEY (product_id) REFERENCES openrails.products(id),
    CONSTRAINT subscriptions_scheduled_price_id_fkey FOREIGN KEY (scheduled_price_id) REFERENCES openrails.prices(id),
    CONSTRAINT subscriptions_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.subscriptions FORCE ROW LEVEL SECURITY;

CREATE TRIGGER trg_subscriptions_set_tier_group BEFORE INSERT OR UPDATE OF product_id ON openrails.subscriptions FOR EACH ROW EXECUTE FUNCTION openrails.subscriptions_set_tier_group();

CREATE INDEX idx_subscriptions_due_dunning ON openrails.subscriptions USING btree (next_retry_at, processor) WHERE ((status = 'past_due'::openrails.subscription_status) AND (next_retry_at IS NOT NULL));
CREATE INDEX idx_subscriptions_grace_ends_at ON openrails.subscriptions USING btree (grace_ends_at) WHERE (grace_ends_at IS NOT NULL);
CREATE INDEX idx_subscriptions_next_retry_at ON openrails.subscriptions USING btree (next_retry_at) WHERE (next_retry_at IS NOT NULL);
CREATE INDEX idx_subscriptions_payment_method_id ON openrails.subscriptions USING btree (payment_method_id);
CREATE INDEX idx_subscriptions_price_id ON openrails.subscriptions USING btree (price_id);
CREATE INDEX idx_subscriptions_processor ON openrails.subscriptions USING btree (processor);
CREATE INDEX idx_subscriptions_processor_subscription ON openrails.subscriptions USING btree (processor, processor_subscription_id);
CREATE INDEX idx_subscriptions_product_id ON openrails.subscriptions USING btree (product_id);
CREATE INDEX idx_subscriptions_status ON openrails.subscriptions USING btree (status);
CREATE INDEX idx_subscriptions_merchant_id ON openrails.subscriptions USING btree (merchant_id);
CREATE INDEX idx_subscriptions_customer ON openrails.subscriptions USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE UNIQUE INDEX uq_subscriptions_merchant_processor_subscription_id ON openrails.subscriptions USING btree (merchant_id, processor, processor_subscription_id) WHERE (processor_subscription_id <> ''::text);
CREATE UNIQUE INDEX uq_subscriptions_customer_product_lifecycle ON openrails.subscriptions USING btree (merchant_id, customer_id, product_id) WHERE (status = ANY (ARRAY['active'::openrails.subscription_status, 'pending'::openrails.subscription_status, 'past_due'::openrails.subscription_status]));
CREATE UNIQUE INDEX uq_subscriptions_customer_tier_group_active ON openrails.subscriptions USING btree (customer_id, tier_group) WHERE ((status = ANY (ARRAY['active'::openrails.subscription_status, 'pending'::openrails.subscription_status])) AND (tier_group IS NOT NULL));

ALTER TABLE openrails.subscriptions ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.subscriptions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.subscriptions TO openrails_app;

COMMENT ON TABLE openrails.subscriptions IS 'Core subscription records tracking user billing relationships';
COMMENT ON COLUMN openrails.subscriptions.product_id IS 'Denormalized product ID for efficient user+product lookups without joining prices';
COMMENT ON COLUMN openrails.subscriptions.scheduled_price_id IS 'Price ID for scheduled tier change (downgrade). Applied at end of current billing period during renewal.';
COMMENT ON COLUMN openrails.subscriptions.tier_group IS 'Denormalized from openrails.products.tier_group (kept in sync by trigger trg_subscriptions_set_tier_group). Backs uq_subscriptions_user_tier_group_active, which enforces one active/pending subscription per (user, tier group).';

-- =============================================================================
-- payments
-- =============================================================================

CREATE TABLE openrails.payments (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid NOT NULL,
    processor openrails.processor_type NOT NULL,
    transaction_id text NOT NULL,
    amount bigint NOT NULL,
    list_amount bigint NOT NULL,
    currency text NOT NULL,
    status openrails.purchase_status DEFAULT 'completed'::openrails.purchase_status NOT NULL,
    subscription_id uuid,
    refunded_payment_id uuid,
    discount_code text,
    discount_reason text,
    discount_metadata jsonb,
    entitlements_spec_snapshot jsonb,
    credits_spec_snapshot jsonb,
    metadata jsonb,
    purchased_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    card_brand text,
    card_last4 text,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    CONSTRAINT payments_pkey PRIMARY KEY (id),
    CONSTRAINT chk_payment_not_future CHECK ((purchased_at <= (now() + '00:05:00'::interval))),
    CONSTRAINT payments_price_id_fkey FOREIGN KEY (price_id) REFERENCES openrails.prices(id),
    CONSTRAINT payments_refunded_payment_id_fkey FOREIGN KEY (refunded_payment_id) REFERENCES openrails.payments(id),
    CONSTRAINT payments_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE SET NULL,
    CONSTRAINT payments_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.payments FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_payments_price_id ON openrails.payments USING btree (price_id);
CREATE INDEX idx_payments_processor ON openrails.payments USING btree (processor);
CREATE INDEX idx_payments_purchased_at ON openrails.payments USING btree (purchased_at);
CREATE INDEX idx_payments_refunded_payment_id ON openrails.payments USING btree (refunded_payment_id);
CREATE INDEX idx_payments_subscription_id ON openrails.payments USING btree (subscription_id);
CREATE INDEX idx_payments_merchant_id ON openrails.payments USING btree (merchant_id);
CREATE INDEX idx_payments_customer ON openrails.payments USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE UNIQUE INDEX uq_payments_merchant_processor_transaction ON openrails.payments USING btree (merchant_id, processor, transaction_id);

ALTER TABLE openrails.payments ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.payments USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payments TO openrails_app;

COMMENT ON TABLE openrails.payments IS 'Records of all payment transactions (formerly purchases table)';
COMMENT ON COLUMN openrails.payments.subscription_id IS 'Links a payment to the subscription that generated it (nullable for one-off payments)';

-- =============================================================================
-- checkout_sessions
-- =============================================================================

CREATE TABLE openrails.checkout_sessions (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid NOT NULL,
    mode text NOT NULL,
    processor text NOT NULL,
    status text NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    expires_at timestamp with time zone,
    reference text,
    transaction_id text,
    payment_id uuid,
    subscription_id uuid,
    processor_fields jsonb,
    processor_state jsonb,
    metadata jsonb,
    idempotency_key text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    CONSTRAINT checkout_sessions_pkey PRIMARY KEY (id),
    CONSTRAINT checkout_sessions_mode_check CHECK ((mode = ANY (ARRAY['one_off'::text, 'subscription'::text]))),
    CONSTRAINT checkout_sessions_payment_id_fkey FOREIGN KEY (payment_id) REFERENCES openrails.payments(id),
    CONSTRAINT checkout_sessions_price_id_fkey FOREIGN KEY (price_id) REFERENCES openrails.prices(id),
    CONSTRAINT checkout_sessions_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id),
    CONSTRAINT checkout_sessions_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.checkout_sessions FORCE ROW LEVEL SECURITY;

CREATE INDEX checkout_sessions_expires_at_idx ON openrails.checkout_sessions USING btree (expires_at);
CREATE UNIQUE INDEX checkout_sessions_processor_reference_idx ON openrails.checkout_sessions USING btree (processor, reference) WHERE (reference IS NOT NULL);
CREATE UNIQUE INDEX checkout_sessions_processor_transaction_id_idx ON openrails.checkout_sessions USING btree (processor, transaction_id) WHERE (transaction_id IS NOT NULL);
CREATE INDEX idx_checkout_sessions_merchant_id ON openrails.checkout_sessions USING btree (merchant_id);
CREATE INDEX idx_checkout_sessions_customer ON openrails.checkout_sessions USING btree (customer_id) WHERE (customer_id IS NOT NULL);

ALTER TABLE openrails.checkout_sessions ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.checkout_sessions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.checkout_sessions TO openrails_app;

-- =============================================================================
-- usdc_funding_sessions
-- =============================================================================

CREATE TABLE openrails.usdc_funding_sessions (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    checkout_session_id uuid,
    provider text NOT NULL,
    wallet_address text NOT NULL,
    asset text NOT NULL,
    network text NOT NULL,
    requested_amount text NOT NULL,
    provider_session_id text,
    provider_url text NOT NULL,
    status text NOT NULL,
    return_url text,
    idempotency_key text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    last_checked_at timestamp with time zone,
    expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT usdc_funding_sessions_pkey PRIMARY KEY (id),
    CONSTRAINT usdc_funding_sessions_asset_valid CHECK ((asset = 'USDC'::text)),
    CONSTRAINT usdc_funding_sessions_nonempty CHECK (((btrim(wallet_address) <> ''::text) AND (btrim(network) <> ''::text) AND (btrim(requested_amount) <> ''::text) AND (btrim(provider_url) <> ''::text))),
    CONSTRAINT usdc_funding_sessions_provider_valid CHECK ((provider = ANY (ARRAY['robinhood'::text, 'coinbase'::text]))),
    CONSTRAINT usdc_funding_sessions_status_valid CHECK ((status = ANY (ARRAY['created'::text, 'opened'::text, 'pending_provider'::text, 'pending_settlement'::text, 'funded'::text, 'failed'::text, 'expired'::text, 'cancelled'::text]))),
    CONSTRAINT usdc_funding_sessions_checkout_session_id_fkey FOREIGN KEY (checkout_session_id) REFERENCES openrails.checkout_sessions(id) ON DELETE SET NULL,
    CONSTRAINT usdc_funding_sessions_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE CASCADE
);

CREATE INDEX idx_usdc_funding_sessions_checkout ON openrails.usdc_funding_sessions USING btree (merchant_id, checkout_session_id) WHERE (checkout_session_id IS NOT NULL);
CREATE UNIQUE INDEX idx_usdc_funding_sessions_idempotency ON openrails.usdc_funding_sessions USING btree (merchant_id, customer_id, idempotency_key) WHERE ((idempotency_key IS NOT NULL) AND (btrim(idempotency_key) <> ''::text));
CREATE INDEX idx_usdc_funding_sessions_provider_session ON openrails.usdc_funding_sessions USING btree (provider, provider_session_id) WHERE (provider_session_id IS NOT NULL);
CREATE INDEX idx_usdc_funding_sessions_customer ON openrails.usdc_funding_sessions USING btree (merchant_id, customer_id, created_at DESC);

ALTER TABLE ONLY openrails.usdc_funding_sessions FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.usdc_funding_sessions ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.usdc_funding_sessions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.usdc_funding_sessions TO openrails_app;

COMMENT ON TABLE openrails.usdc_funding_sessions IS 'External Robinhood/Coinbase handoffs that fund USDC into a user self-custody wallet before normal OpenRails wallet checkout. Return from provider is not proof of funding.';

-- =============================================================================
-- solana_subscriptions
-- =============================================================================

CREATE TABLE openrails.solana_subscriptions (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    subscriber_wallet text NOT NULL,
    authority_pda text NOT NULL,
    subscription_pda text NOT NULL,
    plan_pda text NOT NULL,
    merchant_address text NOT NULL,
    mint text NOT NULL,
    plan_created_at_fingerprint bigint NOT NULL,
    last_pulled_period_start timestamp with time zone,
    last_signature text,
    next_pull_at timestamp with time zone NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT solana_subscriptions_pkey PRIMARY KEY (id),
    CONSTRAINT solana_subscriptions_subscription_pda_key UNIQUE (subscription_pda),
    CONSTRAINT solana_subscriptions_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE CASCADE
);

CREATE INDEX idx_solana_subscriptions_due ON openrails.solana_subscriptions USING btree (merchant_id, next_pull_at) WHERE (status = 'active'::text);
CREATE INDEX idx_solana_subscriptions_subscription_id ON openrails.solana_subscriptions USING btree (subscription_id);

ALTER TABLE ONLY openrails.solana_subscriptions FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.solana_subscriptions ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.solana_subscriptions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.solana_subscriptions TO openrails_app;

-- =============================================================================
-- entitlement_grants
-- =============================================================================

CREATE TABLE openrails.entitlement_grants (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid,
    granted_by text NOT NULL,
    reason text NOT NULL,
    payment_id uuid,
    duration_days integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    CONSTRAINT admin_grants_pkey PRIMARY KEY (id),
    CONSTRAINT admin_grants_payment_id_fkey FOREIGN KEY (payment_id) REFERENCES openrails.payments(id),
    CONSTRAINT admin_grants_price_id_fkey FOREIGN KEY (price_id) REFERENCES openrails.prices(id),
    CONSTRAINT admin_grants_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.entitlement_grants FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_admin_grants_granted_by ON openrails.entitlement_grants USING btree (granted_by);
CREATE INDEX idx_admin_grants_payment_id ON openrails.entitlement_grants USING btree (payment_id) WHERE (payment_id IS NOT NULL);
CREATE INDEX idx_admin_grants_merchant_id ON openrails.entitlement_grants USING btree (merchant_id);
CREATE INDEX idx_admin_grants_customer ON openrails.entitlement_grants USING btree (customer_id) WHERE (customer_id IS NOT NULL);

ALTER TABLE openrails.entitlement_grants ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.entitlement_grants USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.entitlement_grants TO openrails_app;

COMMENT ON TABLE openrails.entitlement_grants IS 'Records admin-initiated product grants (comps, contest winners, manual payments, partnerships)';
COMMENT ON COLUMN openrails.entitlement_grants.price_id IS 'Price/Product being granted - entitlements derived from Product.EntitlementsSpec';
COMMENT ON COLUMN openrails.entitlement_grants.granted_by IS 'Admin user ID who made the grant';
COMMENT ON COLUMN openrails.entitlement_grants.reason IS 'Reason for grant: comp, contest_winner, refund_compensation, partnership, manual_payment, etc.';
COMMENT ON COLUMN openrails.entitlement_grants.payment_id IS 'Optional link to Payment record if money was received';
COMMENT ON COLUMN openrails.entitlement_grants.duration_days IS 'Override entitlement duration (NULL=use Product spec, 0=indefinite, N=N days)';

-- =============================================================================
-- product_access_grants
-- =============================================================================

CREATE TABLE openrails.product_access_grants (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    source_type text NOT NULL,
    source_id text DEFAULT ''::text NOT NULL,
    payment_id uuid,
    status text DEFAULT 'active'::text NOT NULL,
    starts_at timestamp with time zone DEFAULT now() NOT NULL,
    ends_at timestamp with time zone,
    revoked_at timestamp with time zone,
    revoke_reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    customer_id uuid NOT NULL,
    CONSTRAINT product_access_grants_pkey PRIMARY KEY (id),
    CONSTRAINT product_access_grants_source_type_check CHECK ((source_type = ANY (ARRAY['purchase'::text, 'subscription'::text, 'admin'::text]))),
    CONSTRAINT product_access_grants_status_check CHECK ((status = ANY (ARRAY['active'::text, 'revoked'::text]))),
    CONSTRAINT product_access_grants_product_id_fkey FOREIGN KEY (product_id) REFERENCES openrails.products(id),
    CONSTRAINT product_access_grants_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.product_access_grants FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_product_access_grants_customer ON openrails.product_access_grants USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE INDEX ix_product_access_grants_payment ON openrails.product_access_grants USING btree (payment_id) WHERE (payment_id IS NOT NULL);

ALTER TABLE openrails.product_access_grants ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.product_access_grants USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.product_access_grants TO openrails_app;

COMMENT ON TABLE openrails.product_access_grants IS 'Durable, application-facing product ownership/access (issue #250). Distinct from feature entitlements: answers "does this user own product X?" / "list products this user can access". A successful one-time purchase creates a grant; refunds/chargebacks/admin revocation revoke it.';

-- =============================================================================
-- entitlements
-- =============================================================================

CREATE TABLE openrails.entitlements (
    id uuid DEFAULT uuidv7() NOT NULL,
    entitlement text NOT NULL,
    start_at timestamp with time zone NOT NULL,
    end_at timestamp with time zone,
    source_id uuid NOT NULL,
    source_type text NOT NULL,
    revoked_at timestamp with time zone,
    revoke_reason text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    deleted_at timestamp with time zone,
    period tstzrange GENERATED ALWAYS AS (tstzrange(start_at, COALESCE(end_at, 'infinity'::timestamp with time zone), '[)'::text)) STORED,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    CONSTRAINT entitlements_pkey PRIMARY KEY (id),
    CONSTRAINT chk_entitlements_source_type CHECK ((source_type = ANY (ARRAY['subscription'::text, 'one_off'::text, 'admin'::text, 'grace'::text]))),
    CONSTRAINT chk_revoke_fields_together CHECK (((revoked_at IS NULL) = (revoke_reason IS NULL))),
    CONSTRAINT chk_valid_time_window CHECK (((end_at IS NULL) OR (start_at < end_at))),
    CONSTRAINT entitlements_customer_no_overlap EXCLUDE USING gist (merchant_id WITH =, customer_id WITH =, entitlement WITH =, period WITH &&) WHERE (((customer_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL))),
    CONSTRAINT entitlements_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.entitlements FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_entitlements_grace_by_subscription_live ON openrails.entitlements USING btree (source_id, entitlement, start_at, end_at) WHERE ((source_type = 'grace'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX idx_entitlements_live_by_id ON openrails.entitlements USING btree (id) WHERE ((revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX idx_entitlements_one_off_source_live ON openrails.entitlements USING btree (source_id, entitlement) WHERE ((source_type = 'one_off'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX idx_entitlements_source ON openrails.entitlements USING btree (source_type, source_id) WHERE (source_id IS NOT NULL);
CREATE INDEX idx_entitlements_subscription_source_live ON openrails.entitlements USING btree (source_id, entitlement, end_at) WHERE ((source_type = 'subscription'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX idx_entitlements_merchant_id ON openrails.entitlements USING btree (merchant_id);
CREATE INDEX idx_entitlements_customer_active_window ON openrails.entitlements USING btree (merchant_id, customer_id, entitlement, start_at, end_at) WHERE ((customer_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE UNIQUE INDEX uq_entitlements_customer_active ON openrails.entitlements USING btree (merchant_id, customer_id, entitlement) WHERE ((customer_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL) AND (end_at IS NULL));

ALTER TABLE openrails.entitlements ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.entitlements USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.entitlements TO openrails_app;

COMMENT ON COLUMN openrails.entitlements.customer_id IS 'OpenRails payable tenant subject for this entitlement window.';

-- =============================================================================
-- invoices
-- =============================================================================

CREATE TABLE openrails.invoices (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT invoices_customer_id_not_null NOT NULL,
    currency text NOT NULL,
    invoice_number text,
    period_from timestamp with time zone NOT NULL,
    period_to timestamp with time zone NOT NULL,
    usage_total bigint DEFAULT 0 NOT NULL,
    deposits_total bigint DEFAULT 0 NOT NULL,
    owed_accrued bigint DEFAULT 0 NOT NULL,
    owed_paid bigint DEFAULT 0 NOT NULL,
    closing_balance bigint DEFAULT 0 NOT NULL,
    subtotal_amount bigint DEFAULT 0 NOT NULL,
    total_amount bigint DEFAULT 0 NOT NULL,
    amount_paid bigint DEFAULT 0 NOT NULL,
    amount_due bigint DEFAULT 0 NOT NULL,
    line_items jsonb DEFAULT '[]'::jsonb NOT NULL,
    money_movements jsonb DEFAULT '{}'::jsonb NOT NULL,
    status text DEFAULT 'draft'::text NOT NULL,
    collection_method text DEFAULT 'charge_automatically'::text NOT NULL,
    issued_at timestamp with time zone,
    due_at timestamp with time zone,
    paid_at timestamp with time zone,
    voided_at timestamp with time zone,
    uncollectible_at timestamp with time zone,
    sent_at timestamp with time zone,
    finalized_at timestamp with time zone,
    external_invoice_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT invoices_pkey PRIMARY KEY (id),
    CONSTRAINT invoices_amounts_nonneg_chk CHECK (((subtotal_amount >= 0) AND (total_amount >= 0) AND (amount_paid >= 0) AND (amount_due >= 0))),
    CONSTRAINT invoices_collection_method_check CHECK ((collection_method = ANY (ARRAY['charge_automatically'::text, 'send_invoice'::text]))),
    CONSTRAINT invoices_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'open'::text, 'paid'::text, 'past_due'::text, 'voided'::text, 'uncollectible'::text, 'finalized'::text]))),
    CONSTRAINT invoices_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.invoices FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_invoices_customer ON openrails.invoices USING btree (customer_id, period_from DESC);
CREATE INDEX ix_invoices_payer ON openrails.invoices USING btree (merchant_id, customer_id, period_from DESC);
CREATE INDEX ix_invoices_open_due ON openrails.invoices USING btree (merchant_id, customer_id, currency, due_at) WHERE ((status = ANY (ARRAY['open'::text, 'past_due'::text])) AND (amount_due > 0));
CREATE UNIQUE INDEX uq_invoices_period ON openrails.invoices USING btree (merchant_id, customer_id, currency, period_from, period_to);

ALTER TABLE openrails.invoices ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.invoices USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoices TO openrails_app;

COMMENT ON TABLE openrails.invoices IS 'Period invoices/statements. For arrears, an open invoice is the receivable and payments are allocated to it. Prepaid invoices remain informational receipts/statements.';
COMMENT ON COLUMN openrails.invoices.amount_due IS 'Outstanding amount for this invoice in the row currency internal precision. Open arrears balance is derived from open/past-due invoices.';

CREATE TABLE openrails.invoice_items (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT invoice_items_customer_id_not_null NOT NULL,
    currency text NOT NULL,
    invoice_id uuid,
    source_type text NOT NULL,
    source_id text NOT NULL,
    event_type text,
    period_from timestamp with time zone NOT NULL,
    period_to timestamp with time zone NOT NULL,
    invoice_at timestamp with time zone NOT NULL,
    quantity bigint DEFAULT 1 NOT NULL,
    unit_amount bigint DEFAULT 0 NOT NULL,
    amount bigint NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT invoice_items_pkey PRIMARY KEY (id),
    CONSTRAINT invoice_items_amount_nonneg_chk CHECK ((amount >= 0)),
    CONSTRAINT invoice_items_quantity_positive_chk CHECK ((quantity > 0)),
    CONSTRAINT invoice_items_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'invoiced'::text, 'voided'::text]))),
    CONSTRAINT invoice_items_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id),
    CONSTRAINT invoice_items_invoice_fk FOREIGN KEY (invoice_id) REFERENCES openrails.invoices(id) ON DELETE SET NULL
);

ALTER TABLE ONLY openrails.invoice_items FORCE ROW LEVEL SECURITY;

CREATE INDEX ix_invoice_items_pending ON openrails.invoice_items USING btree (merchant_id, customer_id, currency, invoice_at) WHERE ((invoice_id IS NULL) AND (status = 'pending'::text));
CREATE INDEX ix_invoice_items_invoice ON openrails.invoice_items USING btree (merchant_id, invoice_id);
CREATE UNIQUE INDEX uq_invoice_items_source ON openrails.invoice_items USING btree (merchant_id, customer_id, currency, source_type, source_id);

ALTER TABLE openrails.invoice_items ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.invoice_items USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoice_items TO openrails_app;

COMMENT ON TABLE openrails.invoice_items IS 'Pending and invoiced billable items. Arrears usage creates pending items; invoice creation attaches them to a draft/open invoice.';

CREATE TABLE openrails.invoice_payments (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT invoice_payments_customer_id_not_null NOT NULL,
    invoice_id uuid NOT NULL,
    money_transaction_id uuid,
    currency text NOT NULL,
    amount bigint NOT NULL,
    status text DEFAULT 'attempted'::text NOT NULL,
    processor text,
    processor_payment_id text,
    failure_code text,
    failure_message text,
    attempted_at timestamp with time zone DEFAULT now() NOT NULL,
    settled_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT invoice_payments_pkey PRIMARY KEY (id),
    CONSTRAINT invoice_payments_amount_positive_chk CHECK ((amount > 0)),
    CONSTRAINT invoice_payments_status_check CHECK ((status = ANY (ARRAY['attempted'::text, 'settled'::text, 'failed'::text]))),
    CONSTRAINT invoice_payments_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id),
    CONSTRAINT invoice_payments_invoice_fk FOREIGN KEY (invoice_id) REFERENCES openrails.invoices(id) ON DELETE CASCADE
);

ALTER TABLE ONLY openrails.invoice_payments FORCE ROW LEVEL SECURITY;

CREATE INDEX ix_invoice_payments_invoice ON openrails.invoice_payments USING btree (merchant_id, invoice_id, created_at DESC);
CREATE UNIQUE INDEX uq_invoice_payments_money_transaction ON openrails.invoice_payments USING btree (merchant_id, money_transaction_id) WHERE (money_transaction_id IS NOT NULL);

ALTER TABLE openrails.invoice_payments ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.invoice_payments USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoice_payments TO openrails_app;

COMMENT ON TABLE openrails.invoice_payments IS 'Payment attempts and settled payments allocated to a specific invoice.';

-- =============================================================================
-- linked_wallets
-- =============================================================================

CREATE TABLE openrails.linked_wallets (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    chain text NOT NULL,
    address text NOT NULL,
    verification_provider text NOT NULL,
    verified_at timestamp with time zone NOT NULL,
    display_name text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT linked_wallets_pkey PRIMARY KEY (id),
    CONSTRAINT linked_wallets_unique_chain_address UNIQUE (merchant_id, chain, address),
    CONSTRAINT linked_wallets_unique_subject_chain UNIQUE (merchant_id, customer_id, chain),
    CONSTRAINT linked_wallets_chain_address_nonempty CHECK (((btrim(chain) <> ''::text) AND (btrim(address) <> ''::text))),
    CONSTRAINT linked_wallets_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE CASCADE
);

CREATE INDEX idx_linked_wallets_customer ON openrails.linked_wallets USING btree (merchant_id, customer_id);

ALTER TABLE ONLY openrails.linked_wallets FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.linked_wallets ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.linked_wallets USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.linked_wallets TO openrails_app;

COMMENT ON TABLE openrails.linked_wallets IS 'Verified user wallet links for browser self-service billing identity. The wallet must come from trusted delegated-token claims, not request body input.';

-- =============================================================================
-- notification_queue
-- =============================================================================

CREATE TABLE openrails.notification_queue (
    id uuid DEFAULT uuidv7() NOT NULL,
    event_type text NOT NULL,
    data jsonb NOT NULL,
    seen boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    CONSTRAINT notification_queue_pkey PRIMARY KEY (id),
    CONSTRAINT notification_queue_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.notification_queue FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_notification_queue_created_at ON openrails.notification_queue USING btree (created_at);
CREATE INDEX idx_notification_queue_event_type ON openrails.notification_queue USING btree (event_type);
CREATE INDEX idx_notification_queue_seen ON openrails.notification_queue USING btree (seen);
CREATE INDEX idx_notification_queue_merchant_id ON openrails.notification_queue USING btree (merchant_id);
CREATE INDEX idx_notification_queue_customer ON openrails.notification_queue USING btree (customer_id) WHERE (customer_id IS NOT NULL);

ALTER TABLE openrails.notification_queue ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.notification_queue USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.notification_queue TO openrails_app;

COMMENT ON TABLE openrails.notification_queue IS 'Queue for user notifications related to billing and subscriptions';

-- =============================================================================
-- payment_blocklist
-- =============================================================================

CREATE TABLE openrails.payment_blocklist (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid,
    kind text NOT NULL,
    value text NOT NULL,
    reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT payment_blocklist_pkey PRIMARY KEY (id),
    CONSTRAINT payment_blocklist_kind_check CHECK ((kind = ANY (ARRAY['card_fingerprint'::text, 'processor_customer'::text, 'email'::text, 'ip'::text]))),
    CONSTRAINT payment_blocklist_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.payment_blocklist FORCE ROW LEVEL SECURITY;

CREATE UNIQUE INDEX uq_payment_blocklist ON openrails.payment_blocklist USING btree (merchant_id, kind, value);

ALTER TABLE openrails.payment_blocklist ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.payment_blocklist USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payment_blocklist TO openrails_app;

COMMENT ON TABLE openrails.payment_blocklist IS 'Tenant-scoped blocklist of known-bad payment identifiers (issue #300). customer_id NULL = tenant-wide block; set = tenant-subject scoped. Checkout/admission deny wiring is a separate slice.';

-- =============================================================================
-- processor_customers
-- =============================================================================

CREATE TABLE openrails.processor_customers (
    id uuid DEFAULT uuidv7() NOT NULL,
    processor text NOT NULL,
    processor_customer_id text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    CONSTRAINT processor_customers_pkey PRIMARY KEY (id),
    CONSTRAINT processor_customers_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.processor_customers FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_processor_customers_merchant_id ON openrails.processor_customers USING btree (merchant_id);
CREATE INDEX idx_processor_customers_customer ON openrails.processor_customers USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE UNIQUE INDEX uq_processor_customers_merchant_processor_customer ON openrails.processor_customers USING btree (merchant_id, processor, processor_customer_id);
CREATE UNIQUE INDEX uq_processor_customers_customer_processor ON openrails.processor_customers USING btree (merchant_id, customer_id, processor);

ALTER TABLE openrails.processor_customers ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.processor_customers USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.processor_customers TO openrails_app;

-- =============================================================================
-- budget_inflight_holds
-- =============================================================================

CREATE TABLE openrails.budget_inflight_holds (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT budget_inflight_holds_customer_id_not_null NOT NULL,
    invoker_id text CONSTRAINT budget_inflight_holds_invoker_id_not_null NOT NULL,
    currency text NOT NULL,
    amount bigint NOT NULL,
    captured bigint DEFAULT 0 NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    source text NOT NULL,
    source_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    CONSTRAINT budget_inflight_holds_pkey PRIMARY KEY (id),
    CONSTRAINT budget_inflight_holds_status_check CHECK ((status = ANY (ARRAY['active'::text, 'captured'::text, 'released'::text]))),
    CONSTRAINT budget_inflight_holds_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.budget_inflight_holds FORCE ROW LEVEL SECURITY;

CREATE INDEX budget_inflight_holds_window_agg_idx ON openrails.budget_inflight_holds USING btree (merchant_id, customer_id, invoker_id, currency, created_at) WHERE (status = ANY (ARRAY['active'::text, 'captured'::text]));
CREATE INDEX ix_budget_inflight_holds_window ON openrails.budget_inflight_holds USING btree (merchant_id, customer_id, invoker_id, currency, created_at);
CREATE UNIQUE INDEX uq_budget_inflight_holds_idem ON openrails.budget_inflight_holds USING btree (merchant_id, customer_id, invoker_id, currency, source, source_id);

ALTER TABLE openrails.budget_inflight_holds ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.budget_inflight_holds USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.budget_inflight_holds TO openrails_app;

COMMENT ON TABLE openrails.budget_inflight_holds IS 'Per-currency rolling-window budget holds (#494). One row per in-flight/settled charge; used/reserved/remaining are windowed SUM() over created_at within one currency.';
COMMENT ON COLUMN openrails.budget_inflight_holds.invoker_id IS 'Caller-supplied opaque principal string whose rolling money-budget windows are capped.';
COMMENT ON COLUMN openrails.budget_inflight_holds.amount IS 'Reserved amount in the row currency internal precision.';
COMMENT ON COLUMN openrails.budget_inflight_holds.captured IS 'Captured amount in the row currency internal precision.';
COMMENT ON COLUMN openrails.budget_inflight_holds.currency IS 'Native OpenRails currency code; Go registry is the authority.';

-- =============================================================================
-- budget_window_state
-- =============================================================================

CREATE TABLE openrails.budget_window_state (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT budget_window_state_customer_id_not_null NOT NULL,
    invoker_id text CONSTRAINT budget_window_state_invoker_id_not_null NOT NULL,
    currency text NOT NULL,
    window_key text CONSTRAINT budget_window_state_window_key_not_null NOT NULL,
    cadence text NOT NULL,
    window_seconds bigint NOT NULL,
    anchor timestamp with time zone NOT NULL,
    window_start timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT budget_window_state_pkey PRIMARY KEY (id),
    CONSTRAINT budget_window_state_uniq UNIQUE (merchant_id, customer_id, invoker_id, currency, window_key),
    CONSTRAINT budget_window_state_cadence_check CHECK ((cadence = ANY (ARRAY['session'::text, 'fixed'::text]))),
    CONSTRAINT budget_window_state_window_seconds_check CHECK ((window_seconds > 0)),
    CONSTRAINT budget_window_state_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.budget_window_state FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.budget_window_state ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.budget_window_state USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.budget_window_state TO openrails_app;

COMMENT ON TABLE openrails.budget_window_state IS 'Per-(merchant, customer, invoker, currency, window_key) fixed-window anchor (#337/#494). session: window_start rewritten on reopen; fixed: window_start derived from anchor. Locked FOR UPDATE in Reserve as the boundary-rollover serialization point.';
COMMENT ON COLUMN openrails.budget_window_state.anchor IS 'First-ever window open for this (customer, invoker, currency, window_key); fixed-cadence boundaries are anchor + k*window_seconds.';
COMMENT ON COLUMN openrails.budget_window_state.window_start IS 'Start of the most recently OPENED window. Authoritative for session cadence; for fixed cadence the current start is derived from anchor at read time.';

-- =============================================================================
-- invoker_spend_limits
-- =============================================================================

CREATE TABLE openrails.invoker_spend_limits (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT invoker_spend_limits_customer_id_not_null NOT NULL,
    scope text NOT NULL,
    scope_key text DEFAULT ''::text NOT NULL,
    windows jsonb DEFAULT '[]'::jsonb NOT NULL,
    policy_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT invoker_spend_limits_pkey PRIMARY KEY (id),
    CONSTRAINT invoker_spend_limits_scope_check CHECK ((scope = ANY (ARRAY['invoker'::text, 'role'::text, 'invoker_tier'::text]))),
    CONSTRAINT invoker_spend_limits_uniq UNIQUE (merchant_id, customer_id, scope, scope_key),
    CONSTRAINT invoker_spend_limits_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.invoker_spend_limits FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.invoker_spend_limits ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.invoker_spend_limits USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoker_spend_limits TO openrails_app;

COMMENT ON TABLE openrails.invoker_spend_limits IS 'Per-invoker spend limits (#473/#517): the payer caps how much a delegated invoker/role can spend of the payer''s money. {scope, scope_key, windows[]} composed in one admit verdict over the payer balance. Payer-set only.';
COMMENT ON COLUMN openrails.invoker_spend_limits.scope_key IS 'Immutable scope discriminator: role uuid (scope=role), invoker string (scope=invoker), or tier key (scope=invoker_tier).';

-- =============================================================================
-- payer_spend_limits
-- =============================================================================

CREATE TABLE openrails.payer_spend_limits (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid,
    tier text NOT NULL,
    policy jsonb DEFAULT '{}'::jsonb NOT NULL,
    policy_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT payer_spend_limits_pkey PRIMARY KEY (id),
    CONSTRAINT payer_spend_limits_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.payer_spend_limits FORCE ROW LEVEL SECURITY;

CREATE UNIQUE INDEX uq_payer_spend_limits_merchant_default ON openrails.payer_spend_limits USING btree (merchant_id, tier) WHERE (customer_id IS NULL);
CREATE UNIQUE INDEX uq_payer_spend_limits_customer ON openrails.payer_spend_limits USING btree (merchant_id, customer_id, tier) WHERE (customer_id IS NOT NULL);

ALTER TABLE openrails.payer_spend_limits ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.payer_spend_limits USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payer_spend_limits TO openrails_app;

COMMENT ON TABLE openrails.payer_spend_limits IS 'Per-tier payer spend limit (#477/#517): the platform caps the payer''s spend, keyed by trust-tier. customer_id NULL is the merchant-wide default; non-NULL is a per-customer override.';
COMMENT ON COLUMN openrails.payer_spend_limits.customer_id IS 'NULL = merchant-wide default tier limit (#477); non-NULL = per-customer override taking precedence for that customer.';
COMMENT ON COLUMN openrails.payer_spend_limits.policy IS 'JSONB tier money policy: budget_windows and bad_spend_windows. Money values use the request currency internal precision.';

-- =============================================================================
-- tier_schedules
-- =============================================================================

CREATE TABLE openrails.tier_schedules (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid,
    currency text NOT NULL,
    owner text DEFAULT 'platform'::text NOT NULL,
    rungs jsonb DEFAULT '[]'::jsonb NOT NULL,
    schedule_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tier_schedules_pkey PRIMARY KEY (id),
    CONSTRAINT tier_schedules_owner_check CHECK ((owner = ANY (ARRAY['platform'::text, 'subject'::text]))),
    CONSTRAINT tier_schedules_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.tier_schedules FORCE ROW LEVEL SECURITY;

CREATE UNIQUE INDEX uq_tier_schedules_merchant_default ON openrails.tier_schedules USING btree (merchant_id, owner, currency) WHERE (customer_id IS NULL);
CREATE UNIQUE INDEX uq_tier_schedules_customer ON openrails.tier_schedules USING btree (merchant_id, customer_id, owner, currency) WHERE (customer_id IS NOT NULL);

ALTER TABLE openrails.tier_schedules ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.tier_schedules USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.tier_schedules TO openrails_app;

COMMENT ON TABLE openrails.tier_schedules IS 'Persisted tier ladder (#476): rungs declared once per merchant and currency, or as a per-customer/currency override. OpenRails auto-maintains money_settings.tier from same-currency cumulative paid spend unless tier_source=admin.';
COMMENT ON COLUMN openrails.tier_schedules.owner IS 'platform (set by us; subject cannot edit/see) | subject.';
COMMENT ON COLUMN openrails.tier_schedules.customer_id IS 'NULL = merchant-wide default schedule for this currency; non-NULL = per-customer override taking precedence for that customer/currency.';
COMMENT ON COLUMN openrails.tier_schedules.currency IS 'Currency whose cumulative paid amount is compared to this ladder.';
COMMENT ON COLUMN openrails.tier_schedules.rungs IS 'Ordered JSONB array of {tier, min_cumulative_paid_amount}; a payer''s tier = highest rung whose min_cumulative_paid_amount <= same-currency cumulative_paid.';

-- =============================================================================
-- merchant_configurations
-- =============================================================================

CREATE TABLE openrails.merchant_configurations (
    merchant_id uuid NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    config_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT merchant_configurations_pkey PRIMARY KEY (merchant_id),
    CONSTRAINT merchant_configurations_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id)
);

ALTER TABLE ONLY openrails.merchant_configurations FORCE ROW LEVEL SECURITY;

ALTER TABLE openrails.merchant_configurations ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.merchant_configurations USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_configurations TO openrails_app;

COMMENT ON TABLE openrails.merchant_configurations IS 'One merchant-scoped JSON configuration row. Missing keys use service defaults.';
COMMENT ON COLUMN openrails.merchant_configurations.config IS 'JSONB merchant config. delegated_invoker_wasted_spend_windows is an array of {key, window_seconds, limit}; amount values use the request currency internal precision.';

-- =============================================================================
-- usage_events
-- =============================================================================

CREATE TABLE openrails.usage_events (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT usage_events_customer_id_not_null NOT NULL,
    invoker_id text CONSTRAINT usage_events_invoker_id_not_null NOT NULL,
    invoker_type text,
    currency text NOT NULL,
    resource text,
    event_type text NOT NULL,
    dimensions jsonb DEFAULT '{}'::jsonb NOT NULL,
    amount bigint NOT NULL,
    source text NOT NULL,
    source_id text NOT NULL,
    money_transaction_id uuid,
    metadata jsonb,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT usage_events_pkey PRIMARY KEY (id),
    CONSTRAINT usage_events_amount_check CHECK ((amount >= 0)),
    CONSTRAINT usage_events_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.usage_events FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_usage_events_customer_time ON openrails.usage_events USING btree (customer_id, occurred_at);
CREATE INDEX idx_usage_events_invoker ON openrails.usage_events USING btree (merchant_id, invoker_id, occurred_at DESC);
CREATE INDEX ix_usage_events_payer_time ON openrails.usage_events USING btree (merchant_id, customer_id, occurred_at);
CREATE INDEX ix_usage_events_payer_type_time ON openrails.usage_events USING btree (merchant_id, customer_id, event_type, occurred_at);
CREATE UNIQUE INDEX uq_usage_events_idem ON openrails.usage_events USING btree (merchant_id, customer_id, currency, event_type, source, source_id);

ALTER TABLE openrails.usage_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.usage_events USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.usage_events TO openrails_app;

COMMENT ON TABLE openrails.usage_events IS 'Append-only multi-dimensional metered usage (issue #289). Source of truth for usage reporting + #303 invoice line items. Host-priced (amount sent by the host); event + ledger debit commit in one tx. The hot admission path (#298) never reads this table.';
COMMENT ON COLUMN openrails.usage_events.invoker_id IS 'Caller-supplied principal string that fired this metered usage event. Opaque to OpenRails; attribution + grouping only, not a FK. Joins use source/source_id.';
COMMENT ON COLUMN openrails.usage_events.currency IS 'Native OpenRails currency code; amount uses this currency internal precision.';
COMMENT ON COLUMN openrails.usage_events.resource IS 'Caller-supplied free-form string for what was metered (tensorhub: endpoint slug; doujins: plan/item slug). Opaque to OpenRails; nullable, not a FK.';

-- =============================================================================
-- catalog_drift_events
-- =============================================================================

CREATE TABLE openrails.catalog_drift_events (
    id uuid DEFAULT uuidv7() NOT NULL,
    provider text NOT NULL,
    kind text NOT NULL,
    openrails_resource_type text NOT NULL,
    openrails_resource_id text,
    external_resource_id text,
    field text,
    openrails_value text,
    external_value text,
    detected_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    resolved_at timestamp with time zone,
    merchant_id uuid NOT NULL,
    CONSTRAINT catalog_drift_events_pkey PRIMARY KEY (id),
    CONSTRAINT catalog_drift_events_kind_check CHECK ((kind = ANY (ARRAY['orphan_in_stripe'::text, 'missing_in_stripe'::text, 'orphan_in_nmi'::text, 'missing_in_nmi'::text, 'field_drift'::text]))),
    CONSTRAINT catalog_drift_events_openrails_resource_type_check CHECK ((openrails_resource_type = ANY (ARRAY['product'::text, 'price'::text]))),
    CONSTRAINT catalog_drift_events_provider_check CHECK ((provider = ANY (ARRAY['stripe'::text, 'nmi'::text])))
);

ALTER TABLE ONLY openrails.catalog_drift_events FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_catalog_drift_events_open ON openrails.catalog_drift_events USING btree (detected_at DESC) WHERE (resolved_at IS NULL);
CREATE INDEX idx_catalog_drift_events_merchant_id ON openrails.catalog_drift_events USING btree (merchant_id);
CREATE UNIQUE INDEX uq_catalog_drift_open ON openrails.catalog_drift_events USING btree (provider, kind, openrails_resource_type, COALESCE(openrails_resource_id, ''::text), COALESCE(external_resource_id, ''::text), COALESCE(field, ''::text)) WHERE (resolved_at IS NULL);

ALTER TABLE openrails.catalog_drift_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.catalog_drift_events USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_drift_events TO openrails_app;

COMMENT ON TABLE openrails.catalog_drift_events IS 'Alert-only drift/orphan records from the catalog reconciliation loop; resolved via per-price reconcile.';

-- =============================================================================
-- reconciliation_runs
-- =============================================================================

CREATE TABLE openrails.reconciliation_runs (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    mode text NOT NULL,
    providers text[] NOT NULL,
    window_since timestamp with time zone,
    window_until timestamp with time zone,
    started_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    finished_at timestamp with time zone,
    status text DEFAULT 'running'::text NOT NULL,
    summary jsonb,
    error text,
    CONSTRAINT reconciliation_runs_pkey PRIMARY KEY (id),
    CONSTRAINT chk_reconciliation_runs_mode CHECK ((mode = ANY (ARRAY['advisory'::text, 'enforce'::text]))),
    CONSTRAINT chk_reconciliation_runs_status CHECK ((status = ANY (ARRAY['running'::text, 'completed'::text, 'failed'::text])))
);

ALTER TABLE ONLY openrails.reconciliation_runs FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_reconciliation_runs_merchant_id ON openrails.reconciliation_runs USING btree (merchant_id);
CREATE INDEX idx_reconciliation_runs_started_at ON openrails.reconciliation_runs USING btree (started_at DESC);

ALTER TABLE openrails.reconciliation_runs ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.reconciliation_runs USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.reconciliation_runs TO openrails_app;

COMMENT ON TABLE openrails.reconciliation_runs IS 'One row per manual reconcile run (#107): advisory diffs or enforce convergence against the payment processors. Summary jsonb carries per-provider counts and the dunning-forensics report.';

-- =============================================================================
-- reconciliation_findings
-- =============================================================================

CREATE TABLE openrails.reconciliation_findings (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    provider text NOT NULL,
    finding_type text NOT NULL,
    subject_key text NOT NULL,
    severity text NOT NULL,
    status text DEFAULT 'open'::text NOT NULL,
    requires_admin boolean DEFAULT false NOT NULL,
    recommended_action text,
    local_evidence jsonb,
    remote_evidence jsonb,
    intent_evidence jsonb,
    resolution_evidence jsonb,
    first_seen_run uuid NOT NULL,
    last_seen_run uuid NOT NULL,
    first_seen_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    last_seen_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    occurrence_count integer DEFAULT 1 NOT NULL,
    resolved_at timestamp with time zone,
    resolution text,
    notes text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT reconciliation_findings_pkey PRIMARY KEY (id),
    CONSTRAINT chk_reconciliation_findings_type CHECK ((finding_type = ANY (ARRAY['PS-1'::text, 'PS-2'::text, 'PS-3'::text, 'PS-4'::text, 'PS-5'::text, 'PS-6'::text, 'PS-7'::text, 'PS-8'::text, 'PS-9'::text, 'PS-10'::text]))),
    CONSTRAINT chk_reconciliation_findings_severity CHECK ((severity = ANY (ARRAY['critical'::text, 'high'::text, 'medium'::text, 'low'::text]))),
    CONSTRAINT chk_reconciliation_findings_status CHECK ((status = ANY (ARRAY['open'::text, 'auto_fixed'::text, 'admin_pending'::text, 'resolved'::text, 'dismissed'::text]))),
    CONSTRAINT chk_reconciliation_findings_resolution CHECK ((resolution IS NULL OR resolution = ANY (ARRAY['auto_vanished'::text, 'enforced'::text, 'admin_ack'::text, 'dismissed'::text]))),
    CONSTRAINT chk_reconciliation_findings_resolved_fields CHECK (((resolved_at IS NULL) = (resolution IS NULL)))
);

ALTER TABLE ONLY openrails.reconciliation_findings FORCE ROW LEVEL SECURITY;

CREATE UNIQUE INDEX uq_reconciliation_findings_identity ON openrails.reconciliation_findings USING btree (merchant_id, provider, finding_type, subject_key);
CREATE INDEX idx_reconciliation_findings_merchant_id ON openrails.reconciliation_findings USING btree (merchant_id);
CREATE INDEX idx_reconciliation_findings_open ON openrails.reconciliation_findings USING btree (provider, finding_type) WHERE (status = ANY (ARRAY['open'::text, 'admin_pending'::text]));
CREATE INDEX idx_reconciliation_findings_admin_queue ON openrails.reconciliation_findings USING btree (last_seen_at DESC) WHERE (requires_admin AND (status = 'admin_pending'::text));

ALTER TABLE openrails.reconciliation_findings ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.reconciliation_findings USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.reconciliation_findings TO openrails_app;

COMMENT ON TABLE openrails.reconciliation_findings IS 'Durable local-vs-processor drift ledger (#107 PS-1..PS-9). Stable identity per (tenant, provider, finding_type, subject_key): re-runs update, disappearance auto-resolves as auto_vanished. requires_admin = true rows are the admin action queue.';
COMMENT ON COLUMN openrails.reconciliation_findings.subject_key IS 'Stable identity of the drifted subject within (provider, finding_type): processor subscription id, transaction id, local subscription/payment-method uuid, or tenant_subject uuid depending on the check.';
COMMENT ON COLUMN openrails.reconciliation_findings.intent_evidence IS 'Class-3 local-intent annotation (design decision 5): e.g. DeletionScheduledAt marker present => "recorded delete never executed; boot rescan replays it" — keeps the admin queue to genuine unknowns.';

-- =============================================================================
-- probe_verdicts (RLS-exempt by design: instance-level credential state)
-- =============================================================================

CREATE TABLE openrails.probe_verdicts (
    provider text NOT NULL,
    key_hash text NOT NULL,
    verdict text NOT NULL,
    checked_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT probe_verdicts_pkey PRIMARY KEY (provider, key_hash),
    CONSTRAINT chk_probe_verdicts_verdict CHECK ((verdict = ANY (ARRAY['live'::text, 'simulated'::text])))
);

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.probe_verdicts TO openrails_app;

COMMENT ON TABLE openrails.probe_verdicts IS 'Cached NMI test-mode probe verdicts (#348): one row per (provider, sha256(security_key)). Fresh ''live'' refuses boot from cache, fresh ''simulated'' skips the probe, stale/missing re-probes. RLS-exempt by design: instance-level credential state, not tenant data.';
COMMENT ON COLUMN openrails.probe_verdicts.key_hash IS 'sha256 hex of the provider security key. A rotated key hashes differently, so the cache never answers for a credential it has not seen.';

-- =============================================================================
-- provider_intents
-- =============================================================================

CREATE TABLE openrails.provider_intents (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    provider text NOT NULL,
    intent_type text NOT NULL,
    subscription_id uuid,
    payment_id uuid,
    price_id uuid,
    payload jsonb,
    idempotency_key text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    claimed_until timestamp with time zone,
    origin text NOT NULL,
    origin_reason text,
    last_failure_reason text,
    expires_at timestamp with time zone,
    result_evidence jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    executed_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    account_fingerprint text,
    CONSTRAINT provider_intents_pkey PRIMARY KEY (id),
    CONSTRAINT chk_provider_intents_status CHECK ((status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'succeeded'::text, 'unknown_needs_verify'::text, 'failed_retryable'::text, 'failed_terminal'::text, 'superseded'::text, 'expired'::text]))),
    CONSTRAINT chk_provider_intents_origin CHECK ((origin = ANY (ARRAY['user'::text, 'admin'::text, 'system'::text]))),
    CONSTRAINT chk_provider_intents_executed CHECK ((status <> 'succeeded'::text OR executed_at IS NOT NULL))
);

ALTER TABLE ONLY openrails.provider_intents FORCE ROW LEVEL SECURITY;

CREATE UNIQUE INDEX uq_provider_intents_merchant_idempotency_key ON openrails.provider_intents USING btree (merchant_id, idempotency_key);
CREATE INDEX idx_provider_intents_merchant_id ON openrails.provider_intents USING btree (merchant_id);
CREATE INDEX idx_provider_intents_due ON openrails.provider_intents USING btree (next_attempt_at) WHERE (status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'failed_retryable'::text, 'unknown_needs_verify'::text]));
CREATE INDEX idx_provider_intents_subscription ON openrails.provider_intents USING btree (subscription_id) WHERE (subscription_id IS NOT NULL);

ALTER TABLE openrails.provider_intents ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.provider_intents USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.provider_intents TO openrails_app;

COMMENT ON TABLE openrails.provider_intents IS 'Durable, effectively-once outbox for outbound provider mutations (#358). One row per logical intent (unique per tenant on idempotency_key); the executor worker drains whatever is currently executable, the verifier resolves ambiguous outcomes via provider reads.';
COMMENT ON COLUMN openrails.provider_intents.provider IS 'Provider/processor key the mutation targets (e.g. ''mobius'' for an NMI-backed processor, ''stripe'').';
COMMENT ON COLUMN openrails.provider_intents.intent_type IS 'Registry key selecting the per-type semantics (executor, verifier, relevance, backoff): nmi_delete_subscription, and in later phases manual_rebill, refund, plan_archive, ...';
COMMENT ON COLUMN openrails.provider_intents.idempotency_key IS 'Deterministic identity of the logical intent within the tenant. Re-enqueues conflict here: a pending intent is refreshed, a superseded/expired one revived (relevance returned), anything else untouched — effectively-once per logical intent.';
COMMENT ON COLUMN openrails.provider_intents.claimed_until IS 'Single-executor lease (SKIP LOCKED claim). An in_flight row whose lease elapsed was orphaned by a crashed executor and becomes claimable again; per-type execute semantics (verify-then-execute, verifier-before-retry) make the reclaim safe.';
COMMENT ON COLUMN openrails.provider_intents.origin IS 'Who wanted this mutation: user/admin-origin intents execute under mode=limited (reactive completion), system-origin intents require mode=full. Nothing executes under mode=readonly.';
COMMENT ON COLUMN openrails.provider_intents.last_failure_reason IS 'Why the most recent attempt did not succeed (mode parked, kill switch, provider down, declined...). Recorded on the intent, never surfaced as an error.';
COMMENT ON COLUMN openrails.provider_intents.expires_at IS 'End of the relevance window: past this instant the intent expires with a finding instead of firing stale (NULL = relevance governed solely by the type''s relevance check).';
COMMENT ON COLUMN openrails.provider_intents.result_evidence IS 'How the terminal status was established (e.g. {"verified_absent": true} for a delete confirmed by a provider read).';
COMMENT ON COLUMN openrails.provider_intents.account_fingerprint IS 'Fingerprint of the provider account the intent was enqueued against (#365). NULL = pre-guard or unresolvable: executes ungated. Mismatch with the current credentials parks the intent.';

-- =============================================================================
-- money_transactions
-- =============================================================================

CREATE TABLE openrails.money_transactions (
    id uuid DEFAULT uuidv7() NOT NULL,
    invoker_id text CONSTRAINT money_transactions_invoker_id_not_null NOT NULL,
    invoker_type text,
    resource text,
    metadata jsonb,
    amount bigint NOT NULL,
    balance_after bigint,
    transaction_type text NOT NULL,
    source text NOT NULL,
    source_id text,
    expires_at timestamp with time zone,
    description text,
    status text DEFAULT 'posted'::text NOT NULL,
    authorized_amount bigint,
    captured_amount bigint,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT money_transactions_customer_id_not_null NOT NULL,
    currency text NOT NULL,
    invoice_id uuid,
    CONSTRAINT money_transactions_pkey PRIMARY KEY (id),
    CONSTRAINT money_transactions_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id),
    CONSTRAINT money_transactions_invoice_fk FOREIGN KEY (invoice_id) REFERENCES openrails.invoices(id)
);

ALTER TABLE ONLY openrails.money_transactions FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_money_transactions_payer ON openrails.money_transactions USING btree (merchant_id, customer_id, created_at DESC);
CREATE INDEX idx_money_transactions_payer_invoker ON openrails.money_transactions USING btree (merchant_id, customer_id, invoker_id, created_at DESC);
CREATE INDEX idx_money_transactions_merchant_id ON openrails.money_transactions USING btree (merchant_id);
CREATE INDEX ix_money_transactions_invoice ON openrails.money_transactions USING btree (merchant_id, invoice_id) WHERE (invoice_id IS NOT NULL);
CREATE UNIQUE INDEX uniq_money_deposit_idem_payer ON openrails.money_transactions USING btree (merchant_id, customer_id, currency, source, source_id) WHERE ((transaction_type = 'deposit'::text) AND (source_id IS NOT NULL));
CREATE UNIQUE INDEX uniq_money_owed_payment_idem_payer ON openrails.money_transactions USING btree (merchant_id, customer_id, currency, source, source_id) WHERE ((transaction_type = 'owed_payment'::text) AND (source_id IS NOT NULL));
CREATE UNIQUE INDEX uniq_money_withdrawal_idem_payer ON openrails.money_transactions USING btree (merchant_id, customer_id, currency, source, source_id) WHERE ((transaction_type = 'withdrawal'::text) AND (source_id IS NOT NULL));

ALTER TABLE openrails.money_transactions ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.money_transactions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.money_transactions TO openrails_app;

COMMENT ON TABLE openrails.money_transactions IS 'Ledger transactions. Amount values use the row currency internal precision.';
COMMENT ON COLUMN openrails.money_transactions.currency IS 'System currency code (USD/USDC/EUR/JPY/SOL); the Go registry defines the amount precision.';
COMMENT ON COLUMN openrails.money_transactions.metadata IS 'Optional caller-supplied long-tail attribution. Opaque to OpenRails; joins use source/source_id, never these.';
COMMENT ON COLUMN openrails.money_transactions.invoker_id IS 'Caller-supplied principal string that caused this charge. Opaque to OpenRails; used as the per-invoker spend-cap grouping key and as attribution.';
COMMENT ON COLUMN openrails.money_transactions.resource IS 'Caller-supplied free-form string for what the charge was for (charge-A was for resource-B). Opaque to OpenRails; nullable.';

-- =============================================================================
-- money_blocks
-- =============================================================================

CREATE TABLE openrails.money_blocks (
    id uuid DEFAULT uuidv7() NOT NULL,
    original_amount bigint NOT NULL,
    remaining_amount bigint NOT NULL,
    expires_at timestamp with time zone,
    source_transaction_id uuid,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT money_blocks_customer_id_not_null NOT NULL,
    currency text NOT NULL,
    CONSTRAINT money_blocks_pkey PRIMARY KEY (id),
    CONSTRAINT money_blocks_source_transaction_id_fkey FOREIGN KEY (source_transaction_id) REFERENCES openrails.money_transactions(id),
    CONSTRAINT money_blocks_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.money_blocks FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_money_blocks_payer ON openrails.money_blocks USING btree (merchant_id, customer_id, expires_at);
CREATE INDEX idx_money_blocks_merchant_id ON openrails.money_blocks USING btree (merchant_id);

ALTER TABLE openrails.money_blocks ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.money_blocks USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.money_blocks TO openrails_app;

COMMENT ON TABLE openrails.money_blocks IS 'Spendable money blocks. Amount values use the row currency internal precision.';
COMMENT ON COLUMN openrails.money_blocks.currency IS 'System currency code (USD/USDC/EUR/JPY/SOL); the Go registry is the authority.';

-- money_balances was a read-cache roll-up; dropped in #491. available is derived
-- from money_blocks, held from active holds + open windows. This partial index
-- keeps the derived-available SUM cheap.
CREATE INDEX idx_money_blocks_spendable ON openrails.money_blocks USING btree (merchant_id, customer_id, currency) WHERE (remaining_amount > 0);

-- =============================================================================
-- money_settings
-- =============================================================================

CREATE TABLE openrails.money_settings (
    id uuid NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT money_settings_customer_id_not_null NOT NULL,
    billing_mode text DEFAULT 'prepaid'::text NOT NULL,
    max_spend_per_day bigint,
    max_spend_per_month bigint,
    max_outstanding_owed_amount bigint,
    low_balance_threshold bigint,
    auto_topup_enabled boolean DEFAULT false NOT NULL,
    auto_topup_amount_cents bigint,
    auto_topup_payment_method_id uuid,
    default_credit_expiry_days integer,
    hard_stop_on_breach boolean DEFAULT true NOT NULL,
    alert_threshold_pct integer DEFAULT 80 NOT NULL,
    outstanding_owed_amount bigint DEFAULT 0 NOT NULL,
    last_alert_at timestamp with time zone,
    last_topup_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    verified_payment_method boolean DEFAULT false NOT NULL,
    verified_at timestamp with time zone,
    suspended_at timestamp with time zone,
    suspend_reason text,
    tier text,
    tier_source text DEFAULT 'auto'::text NOT NULL,
    currency text NOT NULL,
    credit_limit_amount bigint DEFAULT 0 NOT NULL,
    CONSTRAINT money_settings_pkey PRIMARY KEY (id),
    CONSTRAINT money_settings_alert_pct_chk CHECK (((alert_threshold_pct >= 0) AND (alert_threshold_pct <= 100))),
    CONSTRAINT money_settings_billing_mode_chk CHECK ((billing_mode = ANY (ARRAY['prepaid'::text, 'arrears'::text]))),
    CONSTRAINT money_settings_outstanding_owed_nonneg_chk CHECK ((outstanding_owed_amount >= 0)),
    CONSTRAINT money_settings_tier_source_chk CHECK ((tier_source = ANY (ARRAY['auto'::text, 'admin'::text]))),
    CONSTRAINT money_settings_credit_limit_amount_nonneg_chk CHECK ((credit_limit_amount >= 0)),
    CONSTRAINT money_settings_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.money_settings FORCE ROW LEVEL SECURITY;

CREATE UNIQUE INDEX uq_money_settings_payer ON openrails.money_settings USING btree (merchant_id, customer_id, currency);

ALTER TABLE openrails.money_settings ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.money_settings USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.money_settings TO openrails_app;

COMMENT ON TABLE openrails.money_settings IS 'Per-(merchant, customer, currency) spend policy and money-in config. Amount values use the row currency internal precision.';
COMMENT ON COLUMN openrails.money_settings.currency IS 'System currency code (USD/USDC/EUR/JPY/SOL); the Go registry is the authority.';
COMMENT ON COLUMN openrails.money_settings.max_spend_per_day IS 'Optional daily spend cap in the row currency internal precision; NULL = uncapped.';
COMMENT ON COLUMN openrails.money_settings.max_spend_per_month IS 'Optional monthly spend cap in the row currency internal precision; NULL = uncapped.';
COMMENT ON COLUMN openrails.money_settings.max_outstanding_owed_amount IS 'Optional arrears owed ceiling in the row currency internal precision; NULL = uncapped.';
COMMENT ON COLUMN openrails.money_settings.low_balance_threshold IS 'Optional low-balance trigger in the row currency internal precision.';
COMMENT ON COLUMN openrails.money_settings.outstanding_owed_amount IS 'Current arrears owed amount in the row currency internal precision.';
COMMENT ON COLUMN openrails.money_settings.credit_limit_amount IS 'Admin-set arrears credit line in the row currency internal precision. 0 = no arrears capacity; prepaid balance may still be spent.';
COMMENT ON COLUMN openrails.money_settings.tier_source IS 'auto = tier maintained by tier_schedule auto-graduation; admin = explicit override that auto-graduation must not overwrite.';
COMMENT ON COLUMN openrails.money_settings.verified_payment_method IS 'Legacy metadata noting that a collection method was verified; service admission consumes computed credit capacity instead of checking this flag.';
COMMENT ON COLUMN openrails.money_settings.suspended_at IS 'Legacy account-freeze metadata; service admission does not consult this flag.';

-- =============================================================================
-- money_spend_limits
-- =============================================================================

CREATE TABLE openrails.money_spend_limits (
    id uuid NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT money_spend_limits_customer_id_not_null NOT NULL,
    invoker_id text CONSTRAINT money_spend_limits_invoker_id_not_null NOT NULL,
    invoker_type text,
    max_spend_per_day bigint,
    max_spend_per_month bigint,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    currency text NOT NULL,
    CONSTRAINT money_spend_limits_pkey PRIMARY KEY (id),
    CONSTRAINT money_spend_limits_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.money_spend_limits FORCE ROW LEVEL SECURITY;

CREATE UNIQUE INDEX uq_money_spend_limits_payer_invoker ON openrails.money_spend_limits USING btree (merchant_id, customer_id, currency, invoker_id);

ALTER TABLE openrails.money_spend_limits ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.money_spend_limits USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.money_spend_limits TO openrails_app;

COMMENT ON TABLE openrails.money_spend_limits IS 'Optional per-(invoker, currency) money spend caps for a customer. Amount values use the row currency internal precision.';
COMMENT ON COLUMN openrails.money_spend_limits.currency IS 'System currency code (USD/USDC/EUR/JPY/SOL); the Go registry is the authority.';
COMMENT ON COLUMN openrails.money_spend_limits.invoker_id IS 'Caller-supplied opaque principal string whose spend is capped by this row.';

-- =============================================================================
-- money_windows
-- =============================================================================

CREATE TABLE openrails.money_windows (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid CONSTRAINT money_windows_customer_id_not_null NOT NULL,
    held_amount bigint NOT NULL,
    settled_amount bigint DEFAULT 0 NOT NULL,
    status text DEFAULT 'open'::text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    currency text NOT NULL,
    CONSTRAINT money_windows_pkey PRIMARY KEY (id),
    CONSTRAINT money_windows_status_chk CHECK ((status = ANY (ARRAY['open'::text, 'closed'::text, 'expired'::text]))),
    CONSTRAINT money_windows_held_nonneg_chk CHECK ((held_amount >= 0)),
    CONSTRAINT money_windows_settled_within_held_chk CHECK (((settled_amount >= 0) AND (settled_amount <= held_amount))),
    CONSTRAINT money_windows_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id)
);

ALTER TABLE ONLY openrails.money_windows FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_money_windows_subject_status ON openrails.money_windows USING btree (customer_id, status);
CREATE INDEX idx_money_windows_open_expires ON openrails.money_windows USING btree (expires_at) WHERE (status = 'open'::text);
CREATE INDEX idx_money_windows_merchant_id ON openrails.money_windows USING btree (merchant_id);

ALTER TABLE openrails.money_windows ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.money_windows USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.money_windows TO openrails_app;

COMMENT ON TABLE openrails.money_windows IS 'Prepaid money windows: one bulk held reservation a host admits requests against locally; settled in cross-payer batches, remainder released at close/expiry. Amount values use the row currency internal precision.';
COMMENT ON COLUMN openrails.money_windows.currency IS 'System currency code (USD/USDC/EUR/JPY/SOL); the Go registry is the authority.';
COMMENT ON COLUMN openrails.money_windows.held_amount IS 'Total reserved for this window (open + refills). Held balance is derived from active holds plus open windows.';
COMMENT ON COLUMN openrails.money_windows.settled_amount IS 'Sum of settled actuals. Server enforces settled_amount <= held_amount; the unsettled remainder releases at close/expiry.';

-- =============================================================================
-- custom_credit_types (#475)
-- Per-tenant consumable units (api-credits, gold-coins): tenant-defined, no FX,
-- never billed in. The money ledger's `currency` column references one of these
-- via the qualified code `tenant-slug/name`. decimals separates presentation
-- from integer minor-unit storage (Sui-style): tickets=0, gold=2, micro-usd=6.
-- =============================================================================

CREATE TABLE openrails.custom_credit_types (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    decimals integer DEFAULT 0 NOT NULL,
    active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT custom_credit_types_pkey PRIMARY KEY (id),
    CONSTRAINT custom_credit_types_merchant_name_key UNIQUE (merchant_id, name),
    CONSTRAINT custom_credit_types_decimals_check CHECK ((decimals >= 0) AND (decimals <= 18))
);

ALTER TABLE ONLY openrails.custom_credit_types FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_custom_credit_types_merchant_id ON openrails.custom_credit_types USING btree (merchant_id);

ALTER TABLE openrails.custom_credit_types ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.custom_credit_types USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.custom_credit_types TO openrails_app;

COMMENT ON TABLE openrails.custom_credit_types IS 'Per-tenant custom credit units (#475): consume-only, no FX, never billed in. Referenced from money rows via the qualified code tenant-slug/name.';
COMMENT ON COLUMN openrails.custom_credit_types.decimals IS 'Minor-unit scale for presentation (10^decimals minor units per major unit). Storage is always integer minor units.';

-- =============================================================================
-- Schema-level grants and default privileges
-- =============================================================================

GRANT USAGE ON SCHEMA openrails TO openrails_app;

ALTER DEFAULT PRIVILEGES IN SCHEMA openrails GRANT USAGE, SELECT ON SEQUENCES TO openrails_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA openrails GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO openrails_app;
