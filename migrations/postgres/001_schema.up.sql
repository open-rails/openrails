-- =============================================================================
-- OpenRails billing schema - consolidated final Postgres schema.
--
-- Greenfield hardcut: this structural migration represents the final schema after
-- the historical OpenRails billing migrations through 082. Historical backfills,
-- transition-only renames, and obsolete intermediate objects are intentionally
-- omitted. Production-safe seed rows live in 002_seed.up.sql.
-- =============================================================================

--
--



SET statement_timeout = '300s';
SET lock_timeout = '10s';
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: billing; Type: SCHEMA; Schema: -; Owner: -
--

CREATE SCHEMA IF NOT EXISTS billing;


CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'openrails_app') THEN
        CREATE ROLE openrails_app NOLOGIN NOBYPASSRLS;
    END IF;
END $$;


--
-- Name: processor_type; Type: TYPE; Schema: billing; Owner: -
--

CREATE TYPE billing.processor_type AS ENUM (
    'paypal',
    'solana',
    'mobius',
    'ccbill',
    'stripe',
    'admin',
    'nmi',
    'manual'
);


--
-- Name: purchase_status; Type: TYPE; Schema: billing; Owner: -
--

CREATE TYPE billing.purchase_status AS ENUM (
    'pending',
    'completed',
    'failed',
    'refunded'
);


--
-- Name: subscription_status; Type: TYPE; Schema: billing; Owner: -
--

CREATE TYPE billing.subscription_status AS ENUM (
    'pending',
    'active',
    'expired',
    'cancelled',
    'failed',
    'past_due'
);


--
-- Name: subscriptions_set_tier_group(); Type: FUNCTION; Schema: billing; Owner: -
--

CREATE FUNCTION billing.subscriptions_set_tier_group() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    SELECT prod.tier_group INTO NEW.tier_group
    FROM billing.products AS prod
    WHERE prod.id = NEW.product_id;
    RETURN NEW;
END;
$$;


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: admin_grants; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.admin_grants (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid,
    granted_by text NOT NULL,
    reason text NOT NULL,
    payment_id uuid,
    duration_days integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid NOT NULL
);

ALTER TABLE ONLY billing.admin_grants FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE admin_grants; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.admin_grants IS 'Records admin-initiated product grants (comps, contest winners, manual payments, partnerships)';


--
-- Name: COLUMN admin_grants.price_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.admin_grants.price_id IS 'Price/Product being granted - entitlements derived from Product.EntitlementsSpec';


--
-- Name: COLUMN admin_grants.granted_by; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.admin_grants.granted_by IS 'Admin user ID who made the grant';


--
-- Name: COLUMN admin_grants.reason; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.admin_grants.reason IS 'Reason for grant: comp, contest_winner, refund_compensation, partnership, manual_payment, etc.';


--
-- Name: COLUMN admin_grants.payment_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.admin_grants.payment_id IS 'Optional link to Payment record if money was received';


--
-- Name: COLUMN admin_grants.duration_days; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.admin_grants.duration_days IS 'Override entitlement duration (NULL=use Product spec, 0=indefinite, N=N days)';


--
-- Name: COLUMN admin_grants.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.admin_grants.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: COLUMN admin_grants.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.admin_grants.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: budget_reservations; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.budget_reservations (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL,
    tenant_subject_id uuid CONSTRAINT budget_reservations_tenant_subject_id_not_null NOT NULL,
    invoker_id text CONSTRAINT budget_reservations_actor_not_null NOT NULL,
    amount_millicents bigint NOT NULL,
    captured_millicents bigint DEFAULT 0 NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    source text NOT NULL,
    source_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    CONSTRAINT budget_reservations_status_check CHECK ((status = ANY (ARRAY['active'::text, 'captured'::text, 'released'::text])))
);

ALTER TABLE ONLY billing.budget_reservations FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE budget_reservations; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.budget_reservations IS 'Rolling-window money-budget reservations (issue #304). One row per in-flight/settled charge against an invoker''s passed-in windows; used/reserved/remaining are windowed SUM() over created_at. Idempotent on (tenant, tenant subject, invoker, source, source_id).';


--
-- Name: COLUMN budget_reservations.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.budget_reservations.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: COLUMN budget_reservations.invoker_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.budget_reservations.invoker_id IS 'Principal whose rolling money-budget windows are capped.';


--
-- Name: catalog_drift_events; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.catalog_drift_events (
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
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    CONSTRAINT catalog_drift_events_kind_check CHECK ((kind = ANY (ARRAY['orphan_in_stripe'::text, 'missing_in_stripe'::text, 'orphan_in_nmi'::text, 'missing_in_nmi'::text, 'field_drift'::text]))),
    CONSTRAINT catalog_drift_events_openrails_resource_type_check CHECK ((openrails_resource_type = ANY (ARRAY['product'::text, 'price'::text]))),
    CONSTRAINT catalog_drift_events_provider_check CHECK ((provider = ANY (ARRAY['stripe'::text, 'nmi'::text])))
);

ALTER TABLE ONLY billing.catalog_drift_events FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE catalog_drift_events; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.catalog_drift_events IS 'Alert-only drift/orphan records from the catalog reconciliation loop; resolved via per-price reconcile.';


--
-- Name: COLUMN catalog_drift_events.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.catalog_drift_events.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: checkout_sessions; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.checkout_sessions (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid NOT NULL,
    mode text NOT NULL,
    processor text NOT NULL,
    status text NOT NULL,
    amount bigint NOT NULL,
    currency text DEFAULT 'usd'::text NOT NULL,
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
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid NOT NULL,
    CONSTRAINT checkout_sessions_mode_check CHECK ((mode = ANY (ARRAY['one_off'::text, 'subscription'::text])))
);

ALTER TABLE ONLY billing.checkout_sessions FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN checkout_sessions.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.checkout_sessions.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: COLUMN checkout_sessions.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.checkout_sessions.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: credit_account_settings; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.credit_account_settings (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    tenant_subject_id uuid CONSTRAINT credit_account_settings_tenant_subject_id_not_null NOT NULL,
    credit_type_id uuid NOT NULL,
    billing_mode text DEFAULT 'prepaid'::text NOT NULL,
    max_spend_per_day_cents bigint,
    max_spend_per_month_cents bigint,
    max_outstanding_owed_cents bigint,
    low_balance_threshold_cents bigint,
    auto_topup_enabled boolean DEFAULT false NOT NULL,
    auto_topup_amount_cents bigint,
    auto_topup_payment_method_id uuid,
    default_credit_expiry_days integer,
    hard_stop_on_breach boolean DEFAULT true NOT NULL,
    alert_threshold_pct integer DEFAULT 80 NOT NULL,
    outstanding_owed_cents bigint DEFAULT 0 NOT NULL,
    last_alert_at timestamp with time zone,
    last_topup_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    verified_payment_method boolean DEFAULT false NOT NULL,
    verified_at timestamp with time zone,
    suspended_at timestamp with time zone,
    suspend_reason text,
    tier text,
    CONSTRAINT credit_account_settings_alert_pct_chk CHECK (((alert_threshold_pct >= 0) AND (alert_threshold_pct <= 100))),
    CONSTRAINT credit_account_settings_billing_mode_chk CHECK ((billing_mode = ANY (ARRAY['prepaid'::text, 'arrears'::text]))),
    CONSTRAINT credit_account_settings_owed_nonneg_chk CHECK ((outstanding_owed_cents >= 0))
);

ALTER TABLE ONLY billing.credit_account_settings FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE credit_account_settings; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.credit_account_settings IS 'Per-(tenant, tenant subject, credit_type) spend policy + money-in config (issue #237). Tensorhub SETS these; OpenRails STORES + ENFORCES them.';


--
-- Name: COLUMN credit_account_settings.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_account_settings.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: COLUMN credit_account_settings.verified_payment_method; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_account_settings.verified_payment_method IS 'True once the account has a verified payment method (set after a successful $1 auth-and-void verification charge — issue #299). The charge itself is a separate slice.';


--
-- Name: COLUMN credit_account_settings.suspended_at; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_account_settings.suspended_at IS 'When set, the account is suspended (issue #299). Admission-deny-on-suspended wiring is a separate slice.';


--
-- Name: credit_blocks; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.credit_blocks (
    id uuid DEFAULT uuidv7() NOT NULL,
    invoker_id text CONSTRAINT credit_blocks_invoker_id_not_null NOT NULL,
    credit_type_id uuid NOT NULL,
    original_amount bigint NOT NULL,
    remaining_amount bigint NOT NULL,
    expires_at timestamp with time zone,
    source_transaction_id uuid,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid CONSTRAINT credit_blocks_tenant_subject_id_not_null NOT NULL
);

ALTER TABLE ONLY billing.credit_blocks FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN credit_blocks.invoker_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_blocks.invoker_id IS 'Principal that caused this credit block to be created.';


--
-- Name: COLUMN credit_blocks.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_blocks.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: COLUMN credit_blocks.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_blocks.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: credit_spend_limits; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.credit_spend_limits (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    tenant_subject_id uuid CONSTRAINT credit_spend_limits_tenant_subject_id_not_null NOT NULL,
    credit_type_id uuid NOT NULL,
    invoker_id text CONSTRAINT credit_spend_limits_invoker_not_null NOT NULL,
    max_spend_per_day_cents bigint,
    max_spend_per_month_cents bigint,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY billing.credit_spend_limits FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE credit_spend_limits; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.credit_spend_limits IS 'Optional per-invoker spend caps for a tenant subject (issue #237/#246). invoker_id is the actor.';


--
-- Name: COLUMN credit_spend_limits.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_spend_limits.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: COLUMN credit_spend_limits.invoker_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_spend_limits.invoker_id IS 'Principal whose spend is capped by this row.';


--
-- Name: credit_transactions; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.credit_transactions (
    id uuid DEFAULT uuidv7() NOT NULL,
    invoker_id text CONSTRAINT credit_transactions_invoker_id_not_null NOT NULL,
    credit_type_id uuid NOT NULL,
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
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid CONSTRAINT credit_transactions_tenant_subject_id_not_null NOT NULL
);

ALTER TABLE ONLY billing.credit_transactions FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN credit_transactions.invoker_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_transactions.invoker_id IS 'Principal that invoked the billable operation.';


--
-- Name: COLUMN credit_transactions.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_transactions.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: COLUMN credit_transactions.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_transactions.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: credit_types; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.credit_types (
    id uuid DEFAULT uuidv7() NOT NULL,
    name text NOT NULL,
    display_name text NOT NULL,
    unit text DEFAULT 'usd'::text NOT NULL,
    decimal_places integer DEFAULT 2 NOT NULL,
    is_active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL
);

ALTER TABLE ONLY billing.credit_types FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN credit_types.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.credit_types.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: entitlement_features; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.entitlement_features (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    lookup_key text NOT NULL,
    name text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    metadata jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY billing.entitlement_features FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE entitlement_features; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.entitlement_features IS 'Stripe-shaped first-class entitlement feature definitions (issue #245). lookup_key is the stable value carried in AuthKit JWT entitlements and host-app checks. The internal billing.entitlements window ledger remains the source of truth for active access.';


--
-- Name: entitlements; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.entitlements (
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
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid NOT NULL,
    CONSTRAINT chk_entitlements_source_type CHECK ((source_type = ANY (ARRAY['subscription'::text, 'one_off'::text, 'admin'::text, 'grace'::text]))),
    CONSTRAINT chk_revoke_fields_together CHECK (((revoked_at IS NULL) = (revoke_reason IS NULL))),
    CONSTRAINT chk_valid_time_window CHECK (((end_at IS NULL) OR (start_at < end_at)))
);

ALTER TABLE ONLY billing.entitlements FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN entitlements.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.entitlements.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: COLUMN entitlements.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.entitlements.tenant_subject_id IS 'OpenRails payable tenant subject for this entitlement window.';


--
-- Name: invoices; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.invoices (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL,
    tenant_subject_id uuid CONSTRAINT invoices_tenant_subject_id_not_null NOT NULL,
    credit_type_id uuid NOT NULL,
    currency text DEFAULT ''::text NOT NULL,
    period_from timestamp with time zone NOT NULL,
    period_to timestamp with time zone NOT NULL,
    usage_total bigint DEFAULT 0 NOT NULL,
    deposits_total bigint DEFAULT 0 NOT NULL,
    owed_accrued bigint DEFAULT 0 NOT NULL,
    owed_paid bigint DEFAULT 0 NOT NULL,
    closing_balance bigint DEFAULT 0 NOT NULL,
    line_items jsonb DEFAULT '[]'::jsonb NOT NULL,
    money_movements jsonb DEFAULT '{}'::jsonb NOT NULL,
    status text DEFAULT 'draft'::text NOT NULL,
    finalized_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT invoices_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'finalized'::text, 'voided'::text])))
);

ALTER TABLE ONLY billing.invoices FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE invoices; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.invoices IS 'Monthly itemized statements (issue #303). Line items rolled up from billing.usage_events; money movements from the credit ledger; snapshotted at finalize. Prepaid = receipt, arrears = statement the #301 sweep settles.';


--
-- Name: COLUMN invoices.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.invoices.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: linked_wallets; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.linked_wallets (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid NOT NULL,
    chain text NOT NULL,
    address text NOT NULL,
    verification_provider text NOT NULL,
    verified_at timestamp with time zone NOT NULL,
    display_name text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT linked_wallets_chain_address_nonempty CHECK (((btrim(chain) <> ''::text) AND (btrim(address) <> ''::text)))
);


--
-- Name: TABLE linked_wallets; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.linked_wallets IS 'Verified user wallet links for browser self-service billing identity. The wallet must come from trusted delegated-token claims, not request body input.';


--
-- Name: manual_rebill_attempts; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.manual_rebill_attempts (
    id uuid DEFAULT uuidv7() NOT NULL,
    subscription_id uuid NOT NULL,
    period_end timestamp with time zone NOT NULL,
    processor text NOT NULL,
    order_reference text NOT NULL,
    status text NOT NULL,
    transaction_id text,
    failure_reason text,
    claimed_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    CONSTRAINT manual_rebill_attempts_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'succeeded'::text, 'failed'::text, 'unknown'::text])))
);

ALTER TABLE ONLY billing.manual_rebill_attempts FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN manual_rebill_attempts.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.manual_rebill_attempts.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: notification_queue; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.notification_queue (
    id uuid DEFAULT uuidv7() NOT NULL,
    event_type text NOT NULL,
    data jsonb NOT NULL,
    seen boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid NOT NULL
);

ALTER TABLE ONLY billing.notification_queue FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE notification_queue; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.notification_queue IS 'Queue for user notifications related to billing and subscriptions';


--
-- Name: COLUMN notification_queue.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.notification_queue.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: COLUMN notification_queue.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.notification_queue.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: payment_blocklist; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.payment_blocklist (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL,
    tenant_subject_id uuid,
    kind text NOT NULL,
    value text NOT NULL,
    reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT payment_blocklist_kind_check CHECK ((kind = ANY (ARRAY['card_fingerprint'::text, 'processor_customer'::text, 'email'::text, 'ip'::text])))
);

ALTER TABLE ONLY billing.payment_blocklist FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE payment_blocklist; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.payment_blocklist IS 'Tenant-scoped blocklist of known-bad payment identifiers (issue #300). tenant_subject_id NULL = tenant-wide block; set = tenant-subject scoped. Checkout/admission deny wiring is a separate slice.';


--
-- Name: COLUMN payment_blocklist.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.payment_blocklist.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: payment_methods; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.payment_methods (
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
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid NOT NULL
);

ALTER TABLE ONLY billing.payment_methods FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE payment_methods; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.payment_methods IS 'Generalized payment method table supporting multiple processors.';


--
-- Name: COLUMN payment_methods.processor; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.payment_methods.processor IS 'Payment processor type: nmi, ccbill, stripe, etc.';


--
-- Name: COLUMN payment_methods.vault_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.payment_methods.vault_id IS 'Primary payment method identifier in the processor system';


--
-- Name: COLUMN payment_methods.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.payment_methods.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: COLUMN payment_methods.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.payment_methods.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: payments; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.payments (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid NOT NULL,
    processor billing.processor_type NOT NULL,
    transaction_id text NOT NULL,
    amount bigint NOT NULL,
    list_amount bigint NOT NULL,
    currency text DEFAULT 'usd'::text NOT NULL,
    status billing.purchase_status DEFAULT 'completed'::billing.purchase_status NOT NULL,
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
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid NOT NULL,
    CONSTRAINT chk_payment_not_future CHECK ((purchased_at <= (now() + '00:05:00'::interval)))
);

ALTER TABLE ONLY billing.payments FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE payments; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.payments IS 'Records of all payment transactions (formerly purchases table)';


--
-- Name: COLUMN payments.subscription_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.payments.subscription_id IS 'Links a payment to the subscription that generated it (nullable for one-off payments)';


--
-- Name: COLUMN payments.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.payments.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: COLUMN payments.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.payments.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: platform_audit; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.platform_audit (
    id uuid DEFAULT uuidv7() NOT NULL,
    actor_user_id text NOT NULL,
    actor_tenant text,
    action text NOT NULL,
    target_tenant_id uuid,
    reason text,
    before_state jsonb,
    after_state jsonb,
    detail jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);


--
-- Name: TABLE platform_audit; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.platform_audit IS 'Append-only cross-tenant platform superadmin audit log (issue #226). Records actor, target tenant, action, reason, and before/after state. CROSS-TENANT control-plane state: NOT purged by tenant delete.';


--
-- Name: platform_break_glass; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.platform_break_glass (
    id uuid DEFAULT uuidv7() NOT NULL,
    actor_user_id text NOT NULL,
    target_tenant_id uuid,
    justification text NOT NULL,
    granted_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    CONSTRAINT chk_break_glass_justified CHECK ((length(btrim(justification)) > 0)),
    CONSTRAINT chk_break_glass_window CHECK ((expires_at > granted_at))
);


--
-- Name: TABLE platform_break_glass; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.platform_break_glass IS 'Time-boxed break-glass elevation grants (issue #226). Each grant carries a written justification and an expiry, and is mirrored into platform_audit. CROSS-TENANT control-plane state.';


--
-- Name: prices; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.prices (
    id uuid DEFAULT uuidv7() NOT NULL,
    product_id uuid NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    billing_cycle_days integer,
    processors jsonb,
    status text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    CONSTRAINT prices_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'active'::text, 'archived'::text])))
);

ALTER TABLE ONLY billing.prices FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE prices; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.prices IS 'Pricing tiers for products with processor-specific identifiers';


--
-- Name: COLUMN prices.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.prices.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: processor_customers; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.processor_customers (
    id uuid DEFAULT uuidv7() NOT NULL,
    processor text NOT NULL,
    customer_id text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid NOT NULL
);

ALTER TABLE ONLY billing.processor_customers FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN processor_customers.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.processor_customers.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: COLUMN processor_customers.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.processor_customers.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: product_access_grants; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.product_access_grants (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL,
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
    tenant_subject_id uuid NOT NULL,
    CONSTRAINT product_access_grants_source_type_check CHECK ((source_type = ANY (ARRAY['purchase'::text, 'subscription'::text, 'admin'::text]))),
    CONSTRAINT product_access_grants_status_check CHECK ((status = ANY (ARRAY['active'::text, 'revoked'::text])))
);

ALTER TABLE ONLY billing.product_access_grants FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE product_access_grants; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.product_access_grants IS 'Durable, application-facing product ownership/access (issue #250). Distinct from feature entitlements: answers "does this user own product X?" / "list products this user can access". A successful one-time purchase creates a grant; refunds/chargebacks/admin revocation revoke it.';


--
-- Name: COLUMN product_access_grants.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.product_access_grants.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: product_entitlement_features; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.product_entitlement_features (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    product_id uuid NOT NULL,
    entitlement_feature_id uuid NOT NULL,
    duration_days integer,
    metadata jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY billing.product_entitlement_features FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE product_entitlement_features; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.product_entitlement_features IS 'Stripe-shaped product_feature attachments (issue #245): which entitlement features a product grants when purchased. duration_days null = indefinite.';


--
-- Name: products; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.products (
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
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    CONSTRAINT products_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'active'::text, 'archived'::text])))
);

ALTER TABLE ONLY billing.products FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE products; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.products IS 'Product definitions that can be purchased or subscribed to';


--
-- Name: COLUMN products.credits_spec; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.products.credits_spec IS 'Bundled promo credits spec (amount, expiry, cadence) for subscriptions';


--
-- Name: COLUMN products.tier_group; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.products.tier_group IS 'Semantic group name for mutually-exclusive products (e.g., "premium"). Products in same group require upgrade/downgrade, not parallel ownership.';


--
-- Name: COLUMN products.tier_rank; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.products.tier_rank IS 'Tier ranking within group. Higher = more premium. Used to determine upgrade (higher rank) vs downgrade (lower rank) direction.';


--
-- Name: COLUMN products.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.products.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: solana_subscriptions; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.solana_subscriptions (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
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
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: subscriptions; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.subscriptions (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid,
    product_id uuid NOT NULL,
    status billing.subscription_status DEFAULT 'pending'::billing.subscription_status NOT NULL,
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
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid NOT NULL,
    CONSTRAINT chk_cancelled_has_timestamp CHECK (((status <> 'cancelled'::billing.subscription_status) OR (cancelled_at IS NOT NULL))),
    CONSTRAINT chk_cancelled_has_type CHECK (((status <> 'cancelled'::billing.subscription_status) OR (cancel_type IS NOT NULL))),
    CONSTRAINT chk_cancelled_no_retry_schedule CHECK (((status <> 'cancelled'::billing.subscription_status) OR ((next_retry_at IS NULL) AND (grace_ends_at IS NULL)))),
    CONSTRAINT chk_ended_not_before_cancelled CHECK (((ended_at IS NULL) OR (cancelled_at IS NULL) OR (ended_at >= cancelled_at))),
    CONSTRAINT chk_past_due_has_period_end CHECK (((status <> 'past_due'::billing.subscription_status) OR (current_period_ends_at IS NOT NULL))),
    CONSTRAINT chk_valid_period CHECK (((current_period_starts_at IS NULL) OR (current_period_ends_at IS NULL) OR (current_period_starts_at < current_period_ends_at)))
);

ALTER TABLE ONLY billing.subscriptions FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE subscriptions; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.subscriptions IS 'Core subscription records tracking user billing relationships';


--
-- Name: COLUMN subscriptions.product_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.subscriptions.product_id IS 'Denormalized product ID for efficient user+product lookups without joining prices';


--
-- Name: COLUMN subscriptions.scheduled_price_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.subscriptions.scheduled_price_id IS 'Price ID for scheduled tier change (downgrade). Applied at end of current billing period during renewal.';


--
-- Name: COLUMN subscriptions.tier_group; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.subscriptions.tier_group IS 'Denormalized from billing.products.tier_group (kept in sync by trigger trg_subscriptions_set_tier_group). Backs uq_subscriptions_user_tier_group_active, which enforces one active/pending subscription per (user, tier group).';


--
-- Name: COLUMN subscriptions.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.subscriptions.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: COLUMN subscriptions.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.subscriptions.tenant_subject_id IS 'OpenRails payable tenant subject for this row (#317). Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: tenant_credential_audit; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.tenant_credential_audit (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    action text NOT NULL,
    actor text,
    detail text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT tenant_credential_audit_action_check CHECK ((action = ANY (ARRAY['put'::text, 'rotate'::text, 'delete'::text, 'test'::text])))
);


--
-- Name: TABLE tenant_credential_audit; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.tenant_credential_audit IS 'Append-only audit log of per-tenant credential put/rotate/delete/test events (issue #225).';


--
-- Name: tenant_deks; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.tenant_deks (
    tenant_id uuid NOT NULL,
    wrapped_dek bytea NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);


--
-- Name: TABLE tenant_deks; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.tenant_deks IS 'Wrapped per-tenant Data Encryption Keys for envelope encryption-at-rest (issue #227). wrapped_dek = tenant DEK sealed with the master key (AES-256-GCM, nonce||ct||tag). Master key lives in config/env (self-hosted) or KMS (production), never in the DB. GLOBAL control-plane table.';


--
-- Name: COLUMN tenant_deks.wrapped_dek; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tenant_deks.wrapped_dek IS 'AES-256-GCM(master_key, tenant_dek): nonce(12) || ciphertext(32) || tag(16).';


--
-- Name: tenant_delegated_issuers; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.tenant_delegated_issuers (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL,
    issuer text NOT NULL,
    jwks_uri text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    audiences text[] DEFAULT '{}'::text[] NOT NULL
);


--
-- Name: TABLE tenant_delegated_issuers; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.tenant_delegated_issuers IS 'Federated delegated-token issuer registry (issue #259). Maps a globally-unique token issuer (iss) to the OpenRails tenant it speaks for and the JWKS URL its public keys are fetched from. MANY issuers -> ONE tenant (multiple host apps = distinct keys, one tenant, shared users). GLOBAL control-plane table, not tenant-scoped.';


--
-- Name: COLUMN tenant_delegated_issuers.issuer; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tenant_delegated_issuers.issuer IS 'Token iss value. GLOBALLY UNIQUE -> maps to exactly one tenant (no cross-tenant forgery).';


--
-- Name: COLUMN tenant_delegated_issuers.jwks_uri; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tenant_delegated_issuers.jwks_uri IS 'The ONLY URL OpenRails fetches this issuer''s keys from (allowlist; token-supplied URLs never honored).';


--
-- Name: COLUMN tenant_delegated_issuers.enabled; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tenant_delegated_issuers.enabled IS 'Per-issuer kill-switch. Only enabled rows are loaded into the live verifier; disabling evicts without affecting sibling issuers of the same tenant.';


--
-- Name: COLUMN tenant_delegated_issuers.audiences; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tenant_delegated_issuers.audiences IS 'Accepted OIDC JWT aud values for this tenant issuer.';


--
-- Name: tenant_exports; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.tenant_exports (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL,
    status text DEFAULT 'completed'::text NOT NULL,
    location text,
    row_counts jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT tenant_exports_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'completed'::text, 'failed'::text])))
);


--
-- Name: TABLE tenant_exports; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.tenant_exports IS 'Tenant logical-export bookkeeping (issue #225). Tenant deletion is gated on a completed export row (export-before-delete).';


--
-- Name: tenant_secrets; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.tenant_secrets (
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    value text NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);


--
-- Name: TABLE tenant_secrets; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.tenant_secrets IS 'DB-backed per-tenant secret store (issue #225). Namespaced by (tenant_id, name). The Vault-backed store keeps the same addressing but holds values in Vault. GLOBAL control-plane table.';


--
-- Name: tenant_subjects; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.tenant_subjects (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: TABLE tenant_subjects; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.tenant_subjects IS 'OpenRails payable identity. One row per OIDC-style subject under an OpenRails tenant; billing tables reference this row.';


--
-- Name: COLUMN tenant_subjects.issuer; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tenant_subjects.issuer IS 'OIDC issuer that asserted the subject.';


--
-- Name: COLUMN tenant_subjects.subject; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tenant_subjects.subject IS 'OIDC subject asserted by issuer. May represent a human, company, tenant, service, or chained delegated principal.';


--
-- Name: tenants; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.tenants (
    id uuid DEFAULT uuidv7() NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    authkit_tenant_id text,
    authkit_tenant_slug text,
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
    CONSTRAINT tenants_status_check CHECK ((status = ANY (ARRAY['active'::text, 'suspended'::text, 'deleted'::text])))
);


--
-- Name: TABLE tenants; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.tenants IS 'Tenant / billing-namespace directory. GLOBAL (control-plane) table, not tenant-scoped. Self-hosted installs have exactly one row (slug=default).';


--
-- Name: COLUMN tenants.slug; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tenants.slug IS 'Stable tenant slug used in tenant-scoped routes and resolution. The well-known value ''default'' is the single-tenant / self-hosted namespace.';


--
-- Name: COLUMN tenants.authkit_tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tenants.authkit_tenant_id IS 'OpenRails-owned AuthKit tenant id that operates this tenant (control plane). Nullable until org ownership is wired in #221/#222.';


--
-- Name: COLUMN tenants.billing_tier; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tenants.billing_tier IS 'The platform''s OWN billing tier for this tenant (eats own dogfood, issue #225). Distinct from plan (free-form hosting metadata).';


--
-- Name: COLUMN tenants.webhook_host; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tenants.webhook_host IS 'Optional host an ingress uses to route inbound webhooks to this tenant. OpenRails verifies the signature AFTER tenant resolution (router is not the trust boundary).';


--
-- Name: tier_policies; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.tier_policies (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL,
    tenant_subject_id uuid CONSTRAINT tier_policies_tenant_subject_id_not_null NOT NULL,
    tier text NOT NULL,
    policy jsonb DEFAULT '{}'::jsonb NOT NULL,
    policy_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY billing.tier_policies FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE tier_policies; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.tier_policies IS 'Per-tenant-subject tier throughput policies for the admission check (issue #298). MONEY caps stay in credit_account_settings; rolling money budgets are #304.';


--
-- Name: COLUMN tier_policies.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.tier_policies.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: usage_events; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.usage_events (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL,
    tenant_subject_id uuid CONSTRAINT usage_events_tenant_subject_id_not_null NOT NULL,
    invoker_id text CONSTRAINT usage_events_user_id_not_null NOT NULL,
    credit_type_id uuid NOT NULL,
    event_type text NOT NULL,
    dimensions jsonb DEFAULT '{}'::jsonb NOT NULL,
    amount bigint NOT NULL,
    source text NOT NULL,
    source_id text NOT NULL,
    credit_transaction_id uuid,
    metadata jsonb,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT usage_events_amount_check CHECK ((amount >= 0))
);

ALTER TABLE ONLY billing.usage_events FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE usage_events; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.usage_events IS 'Append-only multi-dimensional metered usage (issue #289). Source of truth for usage reporting + #303 invoice line items. Host-priced (amount sent by the host); event + ledger debit commit in one tx. The hot admission path (#298) never reads this table.';


--
-- Name: COLUMN usage_events.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.usage_events.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: COLUMN usage_events.invoker_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.usage_events.invoker_id IS 'Principal that invoked this metered usage event.';


--
-- Name: usdc_funding_sessions; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.usdc_funding_sessions (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid NOT NULL,
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
    CONSTRAINT usdc_funding_sessions_asset_valid CHECK ((asset = 'USDC'::text)),
    CONSTRAINT usdc_funding_sessions_nonempty CHECK (((btrim(wallet_address) <> ''::text) AND (btrim(network) <> ''::text) AND (btrim(requested_amount) <> ''::text) AND (btrim(provider_url) <> ''::text))),
    CONSTRAINT usdc_funding_sessions_provider_valid CHECK ((provider = ANY (ARRAY['robinhood'::text, 'coinbase'::text]))),
    CONSTRAINT usdc_funding_sessions_status_valid CHECK ((status = ANY (ARRAY['created'::text, 'opened'::text, 'pending_provider'::text, 'pending_settlement'::text, 'funded'::text, 'failed'::text, 'expired'::text, 'cancelled'::text])))
);


--
-- Name: TABLE usdc_funding_sessions; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON TABLE billing.usdc_funding_sessions IS 'External Robinhood/Coinbase handoffs that fund USDC into a user self-custody wallet before normal OpenRails wallet checkout. Return from provider is not proof of funding.';


--
-- Name: user_credit_balances; Type: TABLE; Schema: billing; Owner: -
--

CREATE TABLE billing.user_credit_balances (
    id uuid DEFAULT uuidv7() NOT NULL,
    invoker_id text CONSTRAINT user_credit_balances_user_id_not_null NOT NULL,
    credit_type_id uuid NOT NULL,
    balance bigint DEFAULT 0 NOT NULL,
    held_balance bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid CONSTRAINT user_credit_balances_tenant_subject_id_not_null NOT NULL
);

ALTER TABLE ONLY billing.user_credit_balances FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN user_credit_balances.invoker_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.user_credit_balances.invoker_id IS 'Principal that caused the balance row to be created or updated.';


--
-- Name: COLUMN user_credit_balances.tenant_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.user_credit_balances.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';


--
-- Name: COLUMN user_credit_balances.tenant_subject_id; Type: COMMENT; Schema: billing; Owner: -
--

COMMENT ON COLUMN billing.user_credit_balances.tenant_subject_id IS 'OpenRails payable tenant subject id. Join billing.tenant_subjects for tenant_id, issuer, and subject.';


--
-- Name: admin_grants admin_grants_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.admin_grants
    ADD CONSTRAINT admin_grants_pkey PRIMARY KEY (id);


--
-- Name: budget_reservations budget_reservations_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.budget_reservations
    ADD CONSTRAINT budget_reservations_pkey PRIMARY KEY (id);


--
-- Name: catalog_drift_events catalog_drift_events_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.catalog_drift_events
    ADD CONSTRAINT catalog_drift_events_pkey PRIMARY KEY (id);


--
-- Name: checkout_sessions checkout_sessions_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_pkey PRIMARY KEY (id);


--
-- Name: credit_account_settings credit_account_settings_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_account_settings
    ADD CONSTRAINT credit_account_settings_pkey PRIMARY KEY (id);


--
-- Name: credit_blocks credit_blocks_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_blocks
    ADD CONSTRAINT credit_blocks_pkey PRIMARY KEY (id);


--
-- Name: credit_spend_limits credit_spend_limits_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_spend_limits
    ADD CONSTRAINT credit_spend_limits_pkey PRIMARY KEY (id);


--
-- Name: credit_transactions credit_transactions_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_transactions
    ADD CONSTRAINT credit_transactions_pkey PRIMARY KEY (id);


--
-- Name: credit_types credit_types_name_key; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_types
    ADD CONSTRAINT credit_types_name_key UNIQUE (name);


--
-- Name: credit_types credit_types_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_types
    ADD CONSTRAINT credit_types_pkey PRIMARY KEY (id);


--
-- Name: entitlement_features entitlement_features_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.entitlement_features
    ADD CONSTRAINT entitlement_features_pkey PRIMARY KEY (id);


--
-- Name: entitlement_features entitlement_features_tenant_lookup_key_key; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.entitlement_features
    ADD CONSTRAINT entitlement_features_tenant_lookup_key_key UNIQUE (tenant_id, lookup_key);


--
-- Name: entitlements entitlements_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.entitlements
    ADD CONSTRAINT entitlements_pkey PRIMARY KEY (id);


--
-- Name: entitlements entitlements_tenant_subject_no_overlap; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.entitlements
    ADD CONSTRAINT entitlements_tenant_subject_no_overlap EXCLUDE USING gist (tenant_id WITH =, tenant_subject_id WITH =, entitlement WITH =, period WITH &&) WHERE (((tenant_subject_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL)));


--
-- Name: invoices invoices_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.invoices
    ADD CONSTRAINT invoices_pkey PRIMARY KEY (id);


--
-- Name: linked_wallets linked_wallets_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.linked_wallets
    ADD CONSTRAINT linked_wallets_pkey PRIMARY KEY (id);


--
-- Name: linked_wallets linked_wallets_unique_chain_address; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.linked_wallets
    ADD CONSTRAINT linked_wallets_unique_chain_address UNIQUE (tenant_id, chain, address);


--
-- Name: linked_wallets linked_wallets_unique_subject_chain; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.linked_wallets
    ADD CONSTRAINT linked_wallets_unique_subject_chain UNIQUE (tenant_id, tenant_subject_id, chain);


--
-- Name: manual_rebill_attempts manual_rebill_attempts_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.manual_rebill_attempts
    ADD CONSTRAINT manual_rebill_attempts_pkey PRIMARY KEY (id);


--
-- Name: notification_queue notification_queue_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.notification_queue
    ADD CONSTRAINT notification_queue_pkey PRIMARY KEY (id);


--
-- Name: payment_blocklist payment_blocklist_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.payment_blocklist
    ADD CONSTRAINT payment_blocklist_pkey PRIMARY KEY (id);


--
-- Name: payment_methods payment_methods_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.payment_methods
    ADD CONSTRAINT payment_methods_pkey PRIMARY KEY (id);


--
-- Name: payments payments_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_pkey PRIMARY KEY (id);


--
-- Name: tenant_deks pk_tenant_deks; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenant_deks
    ADD CONSTRAINT pk_tenant_deks PRIMARY KEY (tenant_id);


--
-- Name: tenant_secrets pk_tenant_secrets; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenant_secrets
    ADD CONSTRAINT pk_tenant_secrets PRIMARY KEY (tenant_id, name);


--
-- Name: platform_audit platform_audit_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.platform_audit
    ADD CONSTRAINT platform_audit_pkey PRIMARY KEY (id);


--
-- Name: platform_break_glass platform_break_glass_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.platform_break_glass
    ADD CONSTRAINT platform_break_glass_pkey PRIMARY KEY (id);


--
-- Name: prices prices_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.prices
    ADD CONSTRAINT prices_pkey PRIMARY KEY (id);


--
-- Name: processor_customers processor_customers_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.processor_customers
    ADD CONSTRAINT processor_customers_pkey PRIMARY KEY (id);


--
-- Name: product_access_grants product_access_grants_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.product_access_grants
    ADD CONSTRAINT product_access_grants_pkey PRIMARY KEY (id);


--
-- Name: product_entitlement_features product_entitlement_features_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.product_entitlement_features
    ADD CONSTRAINT product_entitlement_features_pkey PRIMARY KEY (id);


--
-- Name: product_entitlement_features product_entitlement_features_unique; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.product_entitlement_features
    ADD CONSTRAINT product_entitlement_features_unique UNIQUE (tenant_id, product_id, entitlement_feature_id);


--
-- Name: products products_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.products
    ADD CONSTRAINT products_pkey PRIMARY KEY (id);


--
-- Name: products products_slug_key; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.products
    ADD CONSTRAINT products_slug_key UNIQUE (slug);


--
-- Name: solana_subscriptions solana_subscriptions_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_pkey PRIMARY KEY (id);


--
-- Name: solana_subscriptions solana_subscriptions_subscription_pda_key; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_subscription_pda_key UNIQUE (subscription_pda);


--
-- Name: subscriptions subscriptions_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_pkey PRIMARY KEY (id);


--
-- Name: tenant_credential_audit tenant_credential_audit_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenant_credential_audit
    ADD CONSTRAINT tenant_credential_audit_pkey PRIMARY KEY (id);


--
-- Name: tenant_delegated_issuers tenant_delegated_issuers_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenant_delegated_issuers
    ADD CONSTRAINT tenant_delegated_issuers_pkey PRIMARY KEY (id);


--
-- Name: tenant_exports tenant_exports_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenant_exports
    ADD CONSTRAINT tenant_exports_pkey PRIMARY KEY (id);


--
-- Name: tenant_subjects tenant_subjects_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenant_subjects
    ADD CONSTRAINT tenant_subjects_pkey PRIMARY KEY (id);


--
-- Name: tenants tenants_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenants
    ADD CONSTRAINT tenants_pkey PRIMARY KEY (id);


--
-- Name: tier_policies tier_policies_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tier_policies
    ADD CONSTRAINT tier_policies_pkey PRIMARY KEY (id);


--
-- Name: manual_rebill_attempts uniq_manual_rebill_processor_order; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.manual_rebill_attempts
    ADD CONSTRAINT uniq_manual_rebill_processor_order UNIQUE (processor, order_reference);


--
-- Name: manual_rebill_attempts uniq_manual_rebill_subscription_period; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.manual_rebill_attempts
    ADD CONSTRAINT uniq_manual_rebill_subscription_period UNIQUE (subscription_id, period_end);


--
-- Name: prices unique_prices_product_amount_cycle; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.prices
    ADD CONSTRAINT unique_prices_product_amount_cycle UNIQUE (product_id, amount, currency, billing_cycle_days);


--
-- Name: tenant_delegated_issuers uq_tenant_delegated_issuers_issuer; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenant_delegated_issuers
    ADD CONSTRAINT uq_tenant_delegated_issuers_issuer UNIQUE (issuer);


--
-- Name: tenant_subjects uq_tenant_subjects_issuer_subject; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenant_subjects
    ADD CONSTRAINT uq_tenant_subjects_issuer_subject UNIQUE (tenant_id, issuer, subject);


--
-- Name: tenants uq_tenants_slug; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenants
    ADD CONSTRAINT uq_tenants_slug UNIQUE (slug);


--
-- Name: usage_events usage_events_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.usage_events
    ADD CONSTRAINT usage_events_pkey PRIMARY KEY (id);


--
-- Name: usdc_funding_sessions usdc_funding_sessions_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.usdc_funding_sessions
    ADD CONSTRAINT usdc_funding_sessions_pkey PRIMARY KEY (id);


--
-- Name: user_credit_balances user_credit_balances_pkey; Type: CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.user_credit_balances
    ADD CONSTRAINT user_credit_balances_pkey PRIMARY KEY (id);


--
-- Name: checkout_sessions_expires_at_idx; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX checkout_sessions_expires_at_idx ON billing.checkout_sessions USING btree (expires_at);


--
-- Name: checkout_sessions_processor_reference_idx; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX checkout_sessions_processor_reference_idx ON billing.checkout_sessions USING btree (processor, reference) WHERE (reference IS NOT NULL);


--
-- Name: checkout_sessions_processor_transaction_id_idx; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX checkout_sessions_processor_transaction_id_idx ON billing.checkout_sessions USING btree (processor, transaction_id) WHERE (transaction_id IS NOT NULL);


--
-- Name: idx_admin_grants_granted_by; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_admin_grants_granted_by ON billing.admin_grants USING btree (granted_by);


--
-- Name: idx_admin_grants_payment_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_admin_grants_payment_id ON billing.admin_grants USING btree (payment_id) WHERE (payment_id IS NOT NULL);


--
-- Name: idx_admin_grants_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_admin_grants_tenant_id ON billing.admin_grants USING btree (tenant_id);


--
-- Name: idx_admin_grants_tenant_subject; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_admin_grants_tenant_subject ON billing.admin_grants USING btree (tenant_subject_id) WHERE (tenant_subject_id IS NOT NULL);


--
-- Name: idx_break_glass_active; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_break_glass_active ON billing.platform_break_glass USING btree (expires_at) WHERE (revoked_at IS NULL);


--
-- Name: idx_break_glass_actor; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_break_glass_actor ON billing.platform_break_glass USING btree (actor_user_id, expires_at DESC);


--
-- Name: idx_catalog_drift_events_open; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_catalog_drift_events_open ON billing.catalog_drift_events USING btree (detected_at DESC) WHERE (resolved_at IS NULL);


--
-- Name: idx_catalog_drift_events_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_catalog_drift_events_tenant_id ON billing.catalog_drift_events USING btree (tenant_id);


--
-- Name: idx_checkout_sessions_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_checkout_sessions_tenant_id ON billing.checkout_sessions USING btree (tenant_id);


--
-- Name: idx_checkout_sessions_tenant_subject; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_checkout_sessions_tenant_subject ON billing.checkout_sessions USING btree (tenant_subject_id) WHERE (tenant_subject_id IS NOT NULL);


--
-- Name: idx_credit_blocks_payer; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_blocks_payer ON billing.credit_blocks USING btree (tenant_id, tenant_subject_id, credit_type_id, expires_at);


--
-- Name: idx_credit_blocks_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_blocks_tenant_id ON billing.credit_blocks USING btree (tenant_id);


--
-- Name: idx_credit_blocks_tenant_invoker; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_blocks_tenant_invoker ON billing.credit_blocks USING btree (tenant_id, invoker_id, credit_type_id, expires_at);


--
-- Name: idx_credit_blocks_user_expires; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_blocks_user_expires ON billing.credit_blocks USING btree (invoker_id, credit_type_id, expires_at);


--
-- Name: idx_credit_blocks_user_expires_created; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_blocks_user_expires_created ON billing.credit_blocks USING btree (invoker_id, credit_type_id, expires_at, created_at);


--
-- Name: idx_credit_holds_active_expires; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_holds_active_expires ON billing.credit_transactions USING btree (expires_at) WHERE ((transaction_type = 'hold'::text) AND (status = 'active'::text));


--
-- Name: idx_credit_transactions_payer; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_transactions_payer ON billing.credit_transactions USING btree (tenant_id, tenant_subject_id, credit_type_id, created_at DESC);


--
-- Name: idx_credit_transactions_payer_invoker; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_transactions_payer_invoker ON billing.credit_transactions USING btree (tenant_id, tenant_subject_id, credit_type_id, invoker_id, created_at DESC);


--
-- Name: idx_credit_transactions_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_transactions_tenant_id ON billing.credit_transactions USING btree (tenant_id);


--
-- Name: idx_credit_transactions_tenant_invoker; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_transactions_tenant_invoker ON billing.credit_transactions USING btree (tenant_id, invoker_id, credit_type_id, created_at DESC);


--
-- Name: idx_credit_transactions_user_created; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_transactions_user_created ON billing.credit_transactions USING btree (invoker_id, credit_type_id, created_at DESC);


--
-- Name: idx_credit_types_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_credit_types_tenant_id ON billing.credit_types USING btree (tenant_id);


--
-- Name: idx_entitlements_grace_by_subscription_live; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_entitlements_grace_by_subscription_live ON billing.entitlements USING btree (source_id, entitlement, start_at, end_at) WHERE ((source_type = 'grace'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));


--
-- Name: idx_entitlements_live_by_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_entitlements_live_by_id ON billing.entitlements USING btree (id) WHERE ((revoked_at IS NULL) AND (deleted_at IS NULL));


--
-- Name: idx_entitlements_one_off_source_live; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_entitlements_one_off_source_live ON billing.entitlements USING btree (source_id, entitlement) WHERE ((source_type = 'one_off'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));


--
-- Name: idx_entitlements_source; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_entitlements_source ON billing.entitlements USING btree (source_type, source_id) WHERE (source_id IS NOT NULL);


--
-- Name: idx_entitlements_subscription_source_live; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_entitlements_subscription_source_live ON billing.entitlements USING btree (source_id, entitlement, end_at) WHERE ((source_type = 'subscription'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));


--
-- Name: idx_entitlements_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_entitlements_tenant_id ON billing.entitlements USING btree (tenant_id);


--
-- Name: idx_entitlements_tenant_subject_active_window; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_entitlements_tenant_subject_active_window ON billing.entitlements USING btree (tenant_id, tenant_subject_id, entitlement, start_at, end_at) WHERE ((tenant_subject_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL));


--
-- Name: idx_invoices_tenant_subject; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_invoices_tenant_subject ON billing.invoices USING btree (tenant_subject_id, period_from DESC);


--
-- Name: idx_linked_wallets_tenant_subject; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_linked_wallets_tenant_subject ON billing.linked_wallets USING btree (tenant_id, tenant_subject_id);


--
-- Name: idx_manual_rebill_attempts_status_claimed; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_manual_rebill_attempts_status_claimed ON billing.manual_rebill_attempts USING btree (status, claimed_until);


--
-- Name: idx_manual_rebill_attempts_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_manual_rebill_attempts_tenant_id ON billing.manual_rebill_attempts USING btree (tenant_id);


--
-- Name: idx_notification_queue_created_at; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_notification_queue_created_at ON billing.notification_queue USING btree (created_at);


--
-- Name: idx_notification_queue_event_type; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_notification_queue_event_type ON billing.notification_queue USING btree (event_type);


--
-- Name: idx_notification_queue_seen; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_notification_queue_seen ON billing.notification_queue USING btree (seen);


--
-- Name: idx_notification_queue_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_notification_queue_tenant_id ON billing.notification_queue USING btree (tenant_id);


--
-- Name: idx_notification_queue_tenant_subject; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_notification_queue_tenant_subject ON billing.notification_queue USING btree (tenant_subject_id) WHERE (tenant_subject_id IS NOT NULL);


--
-- Name: idx_payment_methods_processor; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_payment_methods_processor ON billing.payment_methods USING btree (processor);


--
-- Name: idx_payment_methods_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_payment_methods_tenant_id ON billing.payment_methods USING btree (tenant_id);


--
-- Name: idx_payment_methods_tenant_subject; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_payment_methods_tenant_subject ON billing.payment_methods USING btree (tenant_subject_id) WHERE (tenant_subject_id IS NOT NULL);


--
-- Name: idx_payment_methods_vault_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_payment_methods_vault_id ON billing.payment_methods USING btree (vault_id);


--
-- Name: idx_payments_price_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_payments_price_id ON billing.payments USING btree (price_id);


--
-- Name: idx_payments_processor; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_payments_processor ON billing.payments USING btree (processor);


--
-- Name: idx_payments_purchased_at; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_payments_purchased_at ON billing.payments USING btree (purchased_at);


--
-- Name: idx_payments_refunded_payment_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_payments_refunded_payment_id ON billing.payments USING btree (refunded_payment_id);


--
-- Name: idx_payments_subscription_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_payments_subscription_id ON billing.payments USING btree (subscription_id);


--
-- Name: idx_payments_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_payments_tenant_id ON billing.payments USING btree (tenant_id);


--
-- Name: idx_payments_tenant_subject; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_payments_tenant_subject ON billing.payments USING btree (tenant_subject_id) WHERE (tenant_subject_id IS NOT NULL);


--
-- Name: idx_platform_audit_action; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_platform_audit_action ON billing.platform_audit USING btree (action, created_at DESC);


--
-- Name: idx_platform_audit_actor; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_platform_audit_actor ON billing.platform_audit USING btree (actor_user_id, created_at DESC);


--
-- Name: idx_platform_audit_target; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_platform_audit_target ON billing.platform_audit USING btree (target_tenant_id, created_at DESC);


--
-- Name: idx_prices_processors; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_prices_processors ON billing.prices USING gin (processors);


--
-- Name: idx_prices_product_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_prices_product_id ON billing.prices USING btree (product_id);


--
-- Name: idx_prices_status; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_prices_status ON billing.prices USING btree (status);


--
-- Name: idx_prices_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_prices_tenant_id ON billing.prices USING btree (tenant_id);


--
-- Name: idx_processor_customers_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_processor_customers_tenant_id ON billing.processor_customers USING btree (tenant_id);


--
-- Name: idx_processor_customers_tenant_subject; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_processor_customers_tenant_subject ON billing.processor_customers USING btree (tenant_subject_id) WHERE (tenant_subject_id IS NOT NULL);


--
-- Name: idx_product_access_grants_tenant_subject; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_product_access_grants_tenant_subject ON billing.product_access_grants USING btree (tenant_subject_id) WHERE (tenant_subject_id IS NOT NULL);


--
-- Name: idx_product_entitlement_features_feature; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_product_entitlement_features_feature ON billing.product_entitlement_features USING btree (tenant_id, entitlement_feature_id);


--
-- Name: idx_product_entitlement_features_product; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_product_entitlement_features_product ON billing.product_entitlement_features USING btree (tenant_id, product_id);


--
-- Name: idx_products_slug; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_products_slug ON billing.products USING btree (slug);


--
-- Name: idx_products_status; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_products_status ON billing.products USING btree (status);


--
-- Name: idx_products_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_products_tenant_id ON billing.products USING btree (tenant_id);


--
-- Name: idx_products_tier_group; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_products_tier_group ON billing.products USING btree (tier_group) WHERE (tier_group IS NOT NULL);


--
-- Name: idx_solana_subscriptions_due; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_solana_subscriptions_due ON billing.solana_subscriptions USING btree (tenant_id, next_pull_at) WHERE (status = 'active'::text);


--
-- Name: idx_solana_subscriptions_subscription_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_solana_subscriptions_subscription_id ON billing.solana_subscriptions USING btree (subscription_id);


--
-- Name: idx_subscriptions_due_dunning; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_subscriptions_due_dunning ON billing.subscriptions USING btree (next_retry_at, processor) WHERE ((status = 'past_due'::billing.subscription_status) AND (next_retry_at IS NOT NULL));


--
-- Name: idx_subscriptions_grace_ends_at; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_subscriptions_grace_ends_at ON billing.subscriptions USING btree (grace_ends_at) WHERE (grace_ends_at IS NOT NULL);


--
-- Name: idx_subscriptions_next_retry_at; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_subscriptions_next_retry_at ON billing.subscriptions USING btree (next_retry_at) WHERE (next_retry_at IS NOT NULL);


--
-- Name: idx_subscriptions_payment_method_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_subscriptions_payment_method_id ON billing.subscriptions USING btree (payment_method_id);


--
-- Name: idx_subscriptions_price_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_subscriptions_price_id ON billing.subscriptions USING btree (price_id);


--
-- Name: idx_subscriptions_processor; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_subscriptions_processor ON billing.subscriptions USING btree (processor);


--
-- Name: idx_subscriptions_processor_subscription; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_subscriptions_processor_subscription ON billing.subscriptions USING btree (processor, processor_subscription_id);


--
-- Name: idx_subscriptions_product_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_subscriptions_product_id ON billing.subscriptions USING btree (product_id);


--
-- Name: idx_subscriptions_status; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_subscriptions_status ON billing.subscriptions USING btree (status);


--
-- Name: idx_subscriptions_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_subscriptions_tenant_id ON billing.subscriptions USING btree (tenant_id);


--
-- Name: idx_subscriptions_tenant_subject; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_subscriptions_tenant_subject ON billing.subscriptions USING btree (tenant_subject_id) WHERE (tenant_subject_id IS NOT NULL);


--
-- Name: idx_tenant_credential_audit_tenant; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_tenant_credential_audit_tenant ON billing.tenant_credential_audit USING btree (tenant_id, created_at DESC);


--
-- Name: idx_tenant_delegated_issuers_enabled; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_tenant_delegated_issuers_enabled ON billing.tenant_delegated_issuers USING btree (enabled) WHERE enabled;


--
-- Name: idx_tenant_delegated_issuers_tenant; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_tenant_delegated_issuers_tenant ON billing.tenant_delegated_issuers USING btree (tenant_id);


--
-- Name: idx_tenant_exports_tenant; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_tenant_exports_tenant ON billing.tenant_exports USING btree (tenant_id, created_at DESC);


--
-- Name: idx_tenant_subjects_tenant; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_tenant_subjects_tenant ON billing.tenant_subjects USING btree (tenant_id);


--
-- Name: idx_usage_events_tenant_subject_time; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_usage_events_tenant_subject_time ON billing.usage_events USING btree (tenant_subject_id, occurred_at);


--
-- Name: idx_usdc_funding_sessions_checkout; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_usdc_funding_sessions_checkout ON billing.usdc_funding_sessions USING btree (tenant_id, checkout_session_id) WHERE (checkout_session_id IS NOT NULL);


--
-- Name: idx_usdc_funding_sessions_idempotency; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX idx_usdc_funding_sessions_idempotency ON billing.usdc_funding_sessions USING btree (tenant_id, tenant_subject_id, idempotency_key) WHERE ((idempotency_key IS NOT NULL) AND (btrim(idempotency_key) <> ''::text));


--
-- Name: idx_usdc_funding_sessions_provider_session; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_usdc_funding_sessions_provider_session ON billing.usdc_funding_sessions USING btree (provider, provider_session_id) WHERE (provider_session_id IS NOT NULL);


--
-- Name: idx_usdc_funding_sessions_tenant_subject; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_usdc_funding_sessions_tenant_subject ON billing.usdc_funding_sessions USING btree (tenant_id, tenant_subject_id, created_at DESC);


--
-- Name: idx_user_credit_balances_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_user_credit_balances_tenant_id ON billing.user_credit_balances USING btree (tenant_id);


--
-- Name: idx_user_credit_balances_tenant_invoker; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_user_credit_balances_tenant_invoker ON billing.user_credit_balances USING btree (tenant_id, invoker_id, credit_type_id);


--
-- Name: idx_user_credit_balances_user; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX idx_user_credit_balances_user ON billing.user_credit_balances USING btree (invoker_id);


--
-- Name: ix_budget_reservations_window; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX ix_budget_reservations_window ON billing.budget_reservations USING btree (tenant_id, tenant_subject_id, invoker_id, created_at);


--
-- Name: ix_invoices_payer; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX ix_invoices_payer ON billing.invoices USING btree (tenant_id, tenant_subject_id, period_from DESC);


--
-- Name: ix_product_access_grants_payment; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX ix_product_access_grants_payment ON billing.product_access_grants USING btree (payment_id) WHERE (payment_id IS NOT NULL);


--
-- Name: ix_usage_events_payer_time; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX ix_usage_events_payer_time ON billing.usage_events USING btree (tenant_id, tenant_subject_id, occurred_at);


--
-- Name: ix_usage_events_payer_type_time; Type: INDEX; Schema: billing; Owner: -
--

CREATE INDEX ix_usage_events_payer_type_time ON billing.usage_events USING btree (tenant_id, tenant_subject_id, event_type, occurred_at);


--
-- Name: uniq_credit_deposit_idem_payer; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uniq_credit_deposit_idem_payer ON billing.credit_transactions USING btree (tenant_id, tenant_subject_id, credit_type_id, source, source_id) WHERE ((transaction_type = 'deposit'::text) AND (source_id IS NOT NULL));


--
-- Name: uniq_credit_hold_idem_payer; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uniq_credit_hold_idem_payer ON billing.credit_transactions USING btree (tenant_id, tenant_subject_id, credit_type_id, source, source_id) WHERE (transaction_type = 'hold'::text);


--
-- Name: uniq_credit_withdrawal_idem_payer; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uniq_credit_withdrawal_idem_payer ON billing.credit_transactions USING btree (tenant_id, tenant_subject_id, credit_type_id, source, source_id) WHERE ((transaction_type = 'withdrawal'::text) AND (source_id IS NOT NULL));


--
-- Name: uniq_manual_rebill_processor_transaction; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uniq_manual_rebill_processor_transaction ON billing.manual_rebill_attempts USING btree (processor, transaction_id) WHERE (transaction_id IS NOT NULL);


--
-- Name: uq_budget_reservations_idem; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_budget_reservations_idem ON billing.budget_reservations USING btree (tenant_id, tenant_subject_id, invoker_id, source, source_id);


--
-- Name: uq_catalog_drift_open; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_catalog_drift_open ON billing.catalog_drift_events USING btree (provider, kind, openrails_resource_type, COALESCE(openrails_resource_id, ''::text), COALESCE(external_resource_id, ''::text), COALESCE(field, ''::text)) WHERE (resolved_at IS NULL);


--
-- Name: uq_credit_account_settings_payer_type; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_credit_account_settings_payer_type ON billing.credit_account_settings USING btree (tenant_id, tenant_subject_id, credit_type_id);


--
-- Name: uq_credit_spend_limits_payer_invoker; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_credit_spend_limits_payer_invoker ON billing.credit_spend_limits USING btree (tenant_id, tenant_subject_id, credit_type_id, invoker_id);


--
-- Name: uq_entitlements_tenant_subject_active; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_entitlements_tenant_subject_active ON billing.entitlements USING btree (tenant_id, tenant_subject_id, entitlement) WHERE ((tenant_subject_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL) AND (end_at IS NULL));


--
-- Name: uq_invoices_period; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_invoices_period ON billing.invoices USING btree (tenant_id, tenant_subject_id, credit_type_id, period_from, period_to);


--
-- Name: uq_payment_blocklist; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_payment_blocklist ON billing.payment_blocklist USING btree (tenant_id, kind, value);


--
-- Name: uq_payment_methods_tenant_processor_vault; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_payment_methods_tenant_processor_vault ON billing.payment_methods USING btree (tenant_id, processor, vault_id);


--
-- Name: uq_payment_methods_tenant_subject_vault; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_payment_methods_tenant_subject_vault ON billing.payment_methods USING btree (tenant_id, tenant_subject_id, vault_id);


--
-- Name: uq_payments_tenant_processor_transaction; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_payments_tenant_processor_transaction ON billing.payments USING btree (tenant_id, processor, transaction_id);


--
-- Name: uq_processor_customers_tenant_processor_customer; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_processor_customers_tenant_processor_customer ON billing.processor_customers USING btree (tenant_id, processor, customer_id);


--
-- Name: uq_processor_customers_tenant_subject_processor; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_processor_customers_tenant_subject_processor ON billing.processor_customers USING btree (tenant_id, tenant_subject_id, processor);


--
-- Name: uq_subscriptions_tenant_processor_subscription_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_subscriptions_tenant_processor_subscription_id ON billing.subscriptions USING btree (tenant_id, processor, processor_subscription_id) WHERE (processor_subscription_id <> ''::text);


--
-- Name: uq_subscriptions_tenant_subject_product_lifecycle; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_subscriptions_tenant_subject_product_lifecycle ON billing.subscriptions USING btree (tenant_id, tenant_subject_id, product_id) WHERE (status = ANY (ARRAY['active'::billing.subscription_status, 'pending'::billing.subscription_status, 'past_due'::billing.subscription_status]));


--
-- Name: uq_subscriptions_tenant_subject_tier_group_active; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_subscriptions_tenant_subject_tier_group_active ON billing.subscriptions USING btree (tenant_subject_id, tier_group) WHERE ((status = ANY (ARRAY['active'::billing.subscription_status, 'pending'::billing.subscription_status])) AND (tier_group IS NOT NULL));


--
-- Name: uq_tenants_authkit_tenant_id; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_tenants_authkit_tenant_id ON billing.tenants USING btree (authkit_tenant_id) WHERE (authkit_tenant_id IS NOT NULL);


--
-- Name: uq_tenants_webhook_host; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_tenants_webhook_host ON billing.tenants USING btree (lower(webhook_host)) WHERE (webhook_host IS NOT NULL);


--
-- Name: uq_tier_policies; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_tier_policies ON billing.tier_policies USING btree (tenant_id, tenant_subject_id, tier);


--
-- Name: uq_usage_events_idem; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_usage_events_idem ON billing.usage_events USING btree (tenant_id, tenant_subject_id, event_type, source, source_id);


--
-- Name: uq_user_credit_balances_payer_type; Type: INDEX; Schema: billing; Owner: -
--

CREATE UNIQUE INDEX uq_user_credit_balances_payer_type ON billing.user_credit_balances USING btree (tenant_id, tenant_subject_id, credit_type_id);


--
-- Name: subscriptions trg_subscriptions_set_tier_group; Type: TRIGGER; Schema: billing; Owner: -
--

CREATE TRIGGER trg_subscriptions_set_tier_group BEFORE INSERT OR UPDATE OF product_id ON billing.subscriptions FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_set_tier_group();


--
-- Name: admin_grants admin_grants_payment_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.admin_grants
    ADD CONSTRAINT admin_grants_payment_id_fkey FOREIGN KEY (payment_id) REFERENCES billing.payments(id);


--
-- Name: admin_grants admin_grants_price_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.admin_grants
    ADD CONSTRAINT admin_grants_price_id_fkey FOREIGN KEY (price_id) REFERENCES billing.prices(id);


--
-- Name: admin_grants admin_grants_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.admin_grants
    ADD CONSTRAINT admin_grants_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: budget_reservations budget_reservations_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.budget_reservations
    ADD CONSTRAINT budget_reservations_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: checkout_sessions checkout_sessions_payment_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_payment_id_fkey FOREIGN KEY (payment_id) REFERENCES billing.payments(id);


--
-- Name: checkout_sessions checkout_sessions_price_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_price_id_fkey FOREIGN KEY (price_id) REFERENCES billing.prices(id);


--
-- Name: checkout_sessions checkout_sessions_subscription_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES billing.subscriptions(id);


--
-- Name: checkout_sessions checkout_sessions_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: credit_account_settings credit_account_settings_credit_type_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_account_settings
    ADD CONSTRAINT credit_account_settings_credit_type_id_fkey FOREIGN KEY (credit_type_id) REFERENCES billing.credit_types(id) ON DELETE CASCADE;


--
-- Name: credit_account_settings credit_account_settings_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_account_settings
    ADD CONSTRAINT credit_account_settings_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: credit_blocks credit_blocks_credit_type_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_blocks
    ADD CONSTRAINT credit_blocks_credit_type_id_fkey FOREIGN KEY (credit_type_id) REFERENCES billing.credit_types(id);


--
-- Name: credit_blocks credit_blocks_source_transaction_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_blocks
    ADD CONSTRAINT credit_blocks_source_transaction_id_fkey FOREIGN KEY (source_transaction_id) REFERENCES billing.credit_transactions(id);


--
-- Name: credit_blocks credit_blocks_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_blocks
    ADD CONSTRAINT credit_blocks_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: credit_spend_limits credit_spend_limits_credit_type_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_spend_limits
    ADD CONSTRAINT credit_spend_limits_credit_type_id_fkey FOREIGN KEY (credit_type_id) REFERENCES billing.credit_types(id) ON DELETE CASCADE;


--
-- Name: credit_spend_limits credit_spend_limits_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_spend_limits
    ADD CONSTRAINT credit_spend_limits_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: credit_transactions credit_transactions_credit_type_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_transactions
    ADD CONSTRAINT credit_transactions_credit_type_id_fkey FOREIGN KEY (credit_type_id) REFERENCES billing.credit_types(id);


--
-- Name: credit_transactions credit_transactions_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.credit_transactions
    ADD CONSTRAINT credit_transactions_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: entitlements entitlements_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.entitlements
    ADD CONSTRAINT entitlements_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: invoices invoices_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.invoices
    ADD CONSTRAINT invoices_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: linked_wallets linked_wallets_tenant_subject_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.linked_wallets
    ADD CONSTRAINT linked_wallets_tenant_subject_id_fkey FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id) ON DELETE CASCADE;


--
-- Name: manual_rebill_attempts manual_rebill_attempts_subscription_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.manual_rebill_attempts
    ADD CONSTRAINT manual_rebill_attempts_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES billing.subscriptions(id);


--
-- Name: notification_queue notification_queue_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.notification_queue
    ADD CONSTRAINT notification_queue_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: payment_blocklist payment_blocklist_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.payment_blocklist
    ADD CONSTRAINT payment_blocklist_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: payment_methods payment_methods_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.payment_methods
    ADD CONSTRAINT payment_methods_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: payments payments_price_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_price_id_fkey FOREIGN KEY (price_id) REFERENCES billing.prices(id);


--
-- Name: payments payments_refunded_payment_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_refunded_payment_id_fkey FOREIGN KEY (refunded_payment_id) REFERENCES billing.payments(id);


--
-- Name: payments payments_subscription_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES billing.subscriptions(id) ON DELETE SET NULL;


--
-- Name: payments payments_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: prices prices_product_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.prices
    ADD CONSTRAINT prices_product_id_fkey FOREIGN KEY (product_id) REFERENCES billing.products(id) ON DELETE RESTRICT;


--
-- Name: processor_customers processor_customers_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.processor_customers
    ADD CONSTRAINT processor_customers_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: product_access_grants product_access_grants_product_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.product_access_grants
    ADD CONSTRAINT product_access_grants_product_id_fkey FOREIGN KEY (product_id) REFERENCES billing.products(id);


--
-- Name: product_access_grants product_access_grants_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.product_access_grants
    ADD CONSTRAINT product_access_grants_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: product_entitlement_features product_entitlement_features_entitlement_feature_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.product_entitlement_features
    ADD CONSTRAINT product_entitlement_features_entitlement_feature_id_fkey FOREIGN KEY (entitlement_feature_id) REFERENCES billing.entitlement_features(id) ON DELETE CASCADE;


--
-- Name: product_entitlement_features product_entitlement_features_product_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.product_entitlement_features
    ADD CONSTRAINT product_entitlement_features_product_id_fkey FOREIGN KEY (product_id) REFERENCES billing.products(id) ON DELETE CASCADE;


--
-- Name: solana_subscriptions solana_subscriptions_subscription_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES billing.subscriptions(id) ON DELETE CASCADE;


--
-- Name: subscriptions subscriptions_payment_method_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_payment_method_id_fkey FOREIGN KEY (payment_method_id) REFERENCES billing.payment_methods(id) ON DELETE SET NULL;


--
-- Name: subscriptions subscriptions_price_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_price_id_fkey FOREIGN KEY (price_id) REFERENCES billing.prices(id);


--
-- Name: subscriptions subscriptions_product_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_product_id_fkey FOREIGN KEY (product_id) REFERENCES billing.products(id);


--
-- Name: subscriptions subscriptions_scheduled_price_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_scheduled_price_id_fkey FOREIGN KEY (scheduled_price_id) REFERENCES billing.prices(id);


--
-- Name: subscriptions subscriptions_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: tenant_delegated_issuers tenant_delegated_issuers_tenant_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenant_delegated_issuers
    ADD CONSTRAINT tenant_delegated_issuers_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES billing.tenants(id) ON DELETE CASCADE;


--
-- Name: tenant_subjects tenant_subjects_tenant_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tenant_subjects
    ADD CONSTRAINT tenant_subjects_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES billing.tenants(id);


--
-- Name: tier_policies tier_policies_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.tier_policies
    ADD CONSTRAINT tier_policies_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: usage_events usage_events_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.usage_events
    ADD CONSTRAINT usage_events_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: usdc_funding_sessions usdc_funding_sessions_checkout_session_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.usdc_funding_sessions
    ADD CONSTRAINT usdc_funding_sessions_checkout_session_id_fkey FOREIGN KEY (checkout_session_id) REFERENCES billing.checkout_sessions(id) ON DELETE SET NULL;


--
-- Name: usdc_funding_sessions usdc_funding_sessions_tenant_subject_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.usdc_funding_sessions
    ADD CONSTRAINT usdc_funding_sessions_tenant_subject_id_fkey FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id) ON DELETE CASCADE;


--
-- Name: user_credit_balances user_credit_balances_credit_type_id_fkey; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.user_credit_balances
    ADD CONSTRAINT user_credit_balances_credit_type_id_fkey FOREIGN KEY (credit_type_id) REFERENCES billing.credit_types(id);


--
-- Name: user_credit_balances user_credit_balances_tenant_subject_fk; Type: FK CONSTRAINT; Schema: billing; Owner: -
--

ALTER TABLE ONLY billing.user_credit_balances
    ADD CONSTRAINT user_credit_balances_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES billing.tenant_subjects(id);


--
-- Name: admin_grants; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.admin_grants ENABLE ROW LEVEL SECURITY;

--
-- Name: budget_reservations; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.budget_reservations ENABLE ROW LEVEL SECURITY;

--
-- Name: catalog_drift_events; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.catalog_drift_events ENABLE ROW LEVEL SECURITY;

--
-- Name: checkout_sessions; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.checkout_sessions ENABLE ROW LEVEL SECURITY;

--
-- Name: credit_account_settings; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.credit_account_settings ENABLE ROW LEVEL SECURITY;

--
-- Name: credit_blocks; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.credit_blocks ENABLE ROW LEVEL SECURITY;

--
-- Name: credit_spend_limits; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.credit_spend_limits ENABLE ROW LEVEL SECURITY;

--
-- Name: credit_transactions; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.credit_transactions ENABLE ROW LEVEL SECURITY;

--
-- Name: credit_types; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.credit_types ENABLE ROW LEVEL SECURITY;

--
-- Name: entitlement_features; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.entitlement_features ENABLE ROW LEVEL SECURITY;

--
-- Name: entitlements; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.entitlements ENABLE ROW LEVEL SECURITY;

--
-- Name: invoices; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.invoices ENABLE ROW LEVEL SECURITY;

--
-- Name: manual_rebill_attempts; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.manual_rebill_attempts ENABLE ROW LEVEL SECURITY;

--
-- Name: notification_queue; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.notification_queue ENABLE ROW LEVEL SECURITY;

--
-- Name: payment_blocklist; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.payment_blocklist ENABLE ROW LEVEL SECURITY;

--
-- Name: payment_methods; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.payment_methods ENABLE ROW LEVEL SECURITY;

--
-- Name: payments; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.payments ENABLE ROW LEVEL SECURITY;

--
-- Name: prices; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.prices ENABLE ROW LEVEL SECURITY;

--
-- Name: processor_customers; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.processor_customers ENABLE ROW LEVEL SECURITY;

--
-- Name: product_access_grants; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.product_access_grants ENABLE ROW LEVEL SECURITY;

--
-- Name: product_entitlement_features; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.product_entitlement_features ENABLE ROW LEVEL SECURITY;

--
-- Name: products; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.products ENABLE ROW LEVEL SECURITY;

--
-- Name: subscriptions; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.subscriptions ENABLE ROW LEVEL SECURITY;

--
-- Name: admin_grants tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.admin_grants USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: budget_reservations tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.budget_reservations USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: catalog_drift_events tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.catalog_drift_events USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: checkout_sessions tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.checkout_sessions USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: credit_account_settings tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.credit_account_settings USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: credit_blocks tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.credit_blocks USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: credit_spend_limits tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.credit_spend_limits USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: credit_transactions tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.credit_transactions USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: credit_types tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.credit_types USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: entitlement_features tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.entitlement_features USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: entitlements tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.entitlements USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: invoices tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.invoices USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: manual_rebill_attempts tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.manual_rebill_attempts USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: notification_queue tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.notification_queue USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: payment_blocklist tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.payment_blocklist USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: payment_methods tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.payment_methods USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: payments tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.payments USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: prices tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.prices USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: processor_customers tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.processor_customers USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: product_access_grants tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.product_access_grants USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: product_entitlement_features tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.product_entitlement_features USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: products tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.products USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: subscriptions tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.subscriptions USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: tier_policies tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.tier_policies USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: usage_events tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.usage_events USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: user_credit_balances tenant_isolation; Type: POLICY; Schema: billing; Owner: -
--

CREATE POLICY tenant_isolation ON billing.user_credit_balances USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));


--
-- Name: tier_policies; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.tier_policies ENABLE ROW LEVEL SECURITY;

--
-- Name: usage_events; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.usage_events ENABLE ROW LEVEL SECURITY;

--
-- Name: user_credit_balances; Type: ROW SECURITY; Schema: billing; Owner: -
--

ALTER TABLE billing.user_credit_balances ENABLE ROW LEVEL SECURITY;

--
-- Name: SCHEMA billing; Type: ACL; Schema: -; Owner: -
--

GRANT USAGE ON SCHEMA billing TO openrails_app;


--
-- Name: TABLE admin_grants; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.admin_grants TO openrails_app;


--
-- Name: TABLE budget_reservations; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.budget_reservations TO openrails_app;


--
-- Name: TABLE catalog_drift_events; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.catalog_drift_events TO openrails_app;


--
-- Name: TABLE checkout_sessions; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.checkout_sessions TO openrails_app;


--
-- Name: TABLE credit_account_settings; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.credit_account_settings TO openrails_app;


--
-- Name: TABLE credit_blocks; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.credit_blocks TO openrails_app;


--
-- Name: TABLE credit_spend_limits; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.credit_spend_limits TO openrails_app;


--
-- Name: TABLE credit_transactions; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.credit_transactions TO openrails_app;


--
-- Name: TABLE credit_types; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.credit_types TO openrails_app;


--
-- Name: TABLE entitlement_features; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.entitlement_features TO openrails_app;


--
-- Name: TABLE entitlements; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.entitlements TO openrails_app;


--
-- Name: TABLE invoices; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.invoices TO openrails_app;


--
-- Name: TABLE linked_wallets; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.linked_wallets TO openrails_app;


--
-- Name: TABLE manual_rebill_attempts; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.manual_rebill_attempts TO openrails_app;


--
-- Name: TABLE notification_queue; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.notification_queue TO openrails_app;


--
-- Name: TABLE payment_blocklist; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.payment_blocklist TO openrails_app;


--
-- Name: TABLE payment_methods; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.payment_methods TO openrails_app;


--
-- Name: TABLE payments; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.payments TO openrails_app;


--
-- Name: TABLE platform_audit; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.platform_audit TO openrails_app;


--
-- Name: TABLE platform_break_glass; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.platform_break_glass TO openrails_app;


--
-- Name: TABLE prices; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.prices TO openrails_app;


--
-- Name: TABLE processor_customers; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.processor_customers TO openrails_app;


--
-- Name: TABLE product_access_grants; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.product_access_grants TO openrails_app;


--
-- Name: TABLE product_entitlement_features; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.product_entitlement_features TO openrails_app;


--
-- Name: TABLE products; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.products TO openrails_app;


--
-- Name: TABLE solana_subscriptions; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.solana_subscriptions TO openrails_app;


--
-- Name: TABLE subscriptions; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.subscriptions TO openrails_app;


--
-- Name: TABLE tenant_credential_audit; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.tenant_credential_audit TO openrails_app;


--
-- Name: TABLE tenant_deks; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.tenant_deks TO openrails_app;


--
-- Name: TABLE tenant_delegated_issuers; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.tenant_delegated_issuers TO openrails_app;


--
-- Name: TABLE tenant_exports; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.tenant_exports TO openrails_app;


--
-- Name: TABLE tenant_secrets; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.tenant_secrets TO openrails_app;


--
-- Name: TABLE tenant_subjects; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.tenant_subjects TO openrails_app;


--
-- Name: TABLE tenants; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.tenants TO openrails_app;


--
-- Name: TABLE tier_policies; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.tier_policies TO openrails_app;


--
-- Name: TABLE usage_events; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.usage_events TO openrails_app;


--
-- Name: TABLE usdc_funding_sessions; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.usdc_funding_sessions TO openrails_app;


--
-- Name: TABLE user_credit_balances; Type: ACL; Schema: billing; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.user_credit_balances TO openrails_app;


--
-- Name: DEFAULT PRIVILEGES FOR SEQUENCES; Type: DEFAULT ACL; Schema: billing; Owner: -
--

ALTER DEFAULT PRIVILEGES IN SCHEMA billing GRANT USAGE, SELECT ON SEQUENCES TO openrails_app;


--
-- Name: DEFAULT PRIVILEGES FOR TABLES; Type: DEFAULT ACL; Schema: billing; Owner: -
--

ALTER DEFAULT PRIVILEGES IN SCHEMA billing GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO openrails_app;


--
--
