-- =============================================================================
-- OpenRails billing schema — consolidated baseline of migrations 001..029.
--
-- Mechanically produced by pg_dump --schema-only --no-owner from a fully-migrated
-- database (migrations 001..029 applied in order). See git history for the
-- original incremental migrations that were squashed into this baseline.
--
-- Consolidated as of 2026-06-24 (Branch A: greenfield / all environments fully
-- migrated through migration 029). Every existing database already has this schema
-- and will skip this migration harmlessly (migratekit tracks by filename prefix,
-- no checksum). Fresh databases get the full final schema from this single file.
--
-- DO NOT hand-edit object definitions. If the schema must change, add a new
-- migration (002_*.up.sql) rather than modifying this baseline.
-- =============================================================================

SET statement_timeout = '300s';
SET lock_timeout = '10s';
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'openrails_app') THEN
        CREATE ROLE openrails_app NOLOGIN NOBYPASSRLS;
    END IF;
END $$;

-- Extensions required by the schema (btree_gist for EXCLUDE constraints on uuid+tstzrange)
CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

--
-- Name: rail_type; Type: TYPE; Schema: openrails; Owner: -
--

CREATE TYPE openrails.rail_type AS ENUM (
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
-- Name: purchase_status; Type: TYPE; Schema: openrails; Owner: -
--

CREATE TYPE openrails.purchase_status AS ENUM (
    'pending',
    'completed',
    'failed',
    'refunded'
);


--
-- Name: subscription_status; Type: TYPE; Schema: openrails; Owner: -
--

CREATE TYPE openrails.subscription_status AS ENUM (
    'pending',
    'active',
    'expired',
    'cancelled',
    'failed',
    'past_due'
);


--
-- Name: ledger_transfers_apply_counters(); Type: FUNCTION; Schema: openrails; Owner: -
--

CREATE FUNCTION openrails.ledger_transfers_apply_counters() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
DECLARE
    acc openrails.ledger_accounts%ROWTYPE;
    debit openrails.ledger_accounts%ROWTYPE;
    credit openrails.ledger_accounts%ROWTYPE;
    debit_balance bigint;
    credit_balance bigint;
BEGIN
    FOR acc IN
        SELECT *
        FROM openrails.ledger_accounts
        WHERE merchant_id = NEW.merchant_id
          AND id IN (NEW.debit_account_id, NEW.credit_account_id)
        ORDER BY id
        FOR UPDATE
    LOOP
        IF acc.id = NEW.debit_account_id THEN
            debit := acc;
        ELSIF acc.id = NEW.credit_account_id THEN
            credit := acc;
        END IF;
    END LOOP;

    IF debit.id IS NULL OR credit.id IS NULL THEN
        RAISE EXCEPTION 'ledger_transfers: debit/credit account not found';
    END IF;

    IF debit.currency <> NEW.currency OR credit.currency <> NEW.currency THEN
        RAISE EXCEPTION 'ledger_transfers: cross-currency transfer (debit=%, credit=%, transfer=%) - a transfer never crosses ledgers', debit.currency, credit.currency, NEW.currency;
    END IF;

    debit_balance := debit.credits_posted - debit.debits_posted - NEW.amount;
    credit_balance := credit.debits_posted - credit.credits_posted - NEW.amount;
    IF debit.debits_must_not_exceed_credits AND debit_balance < -NEW.allow_debit_negative_up_to THEN
        RAISE EXCEPTION 'ledger_insufficient_funds: balance %, amount %, floor %', debit.credits_posted - debit.debits_posted, NEW.amount, NEW.allow_debit_negative_up_to;
    END IF;
    IF credit.credits_must_not_exceed_debits AND credit_balance < 0 THEN
        RAISE EXCEPTION 'ledger_credit_constraint: credit account % would exceed debits', NEW.credit_account_id;
    END IF;

    UPDATE openrails.ledger_accounts
    SET debits_posted = debits_posted + NEW.amount
    WHERE id = NEW.debit_account_id;
    UPDATE openrails.ledger_accounts
    SET credits_posted = credits_posted + NEW.amount
    WHERE id = NEW.credit_account_id;

    RETURN NEW;
END;
$$;


--
-- Name: subscriptions_set_tier_group(); Type: FUNCTION; Schema: openrails; Owner: -
--

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

--
-- Name: bootstrap_state; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.bootstrap_state (
    singleton boolean DEFAULT true NOT NULL,
    applied_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT bootstrap_state_singleton CHECK (singleton)
);


--
-- Name: catalog_drift_events; Type: TABLE; Schema: openrails; Owner: -
--

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
    CONSTRAINT catalog_drift_events_kind_check CHECK ((kind = ANY (ARRAY['orphan_in_stripe'::text, 'missing_in_stripe'::text, 'orphan_in_nmi'::text, 'missing_in_nmi'::text, 'field_drift'::text]))),
    CONSTRAINT catalog_drift_events_openrails_resource_type_check CHECK ((openrails_resource_type = ANY (ARRAY['product'::text, 'price'::text]))),
    CONSTRAINT catalog_drift_events_provider_check CHECK ((provider = ANY (ARRAY['stripe'::text, 'nmi'::text])))
);

ALTER TABLE ONLY openrails.catalog_drift_events FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE catalog_drift_events; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.catalog_drift_events IS 'Alert-only drift/orphan records from the catalog reconciliation loop; resolved via per-price reconcile.';


--
-- Name: checkout_sessions; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.checkout_sessions (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid NOT NULL,
    mode text NOT NULL,
    rail text NOT NULL,
    status text NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    expires_at timestamp with time zone,
    reference text,
    transaction_id text,
    payment_id uuid,
    subscription_id uuid,
    rail_fields jsonb,
    rail_state jsonb,
    metadata jsonb,
    idempotency_key text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    provider_account_id uuid,
    CONSTRAINT checkout_sessions_mode_check CHECK ((mode = ANY (ARRAY['one_off'::text, 'subscription'::text])))
);

ALTER TABLE ONLY openrails.checkout_sessions FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN checkout_sessions.provider_account_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.checkout_sessions.provider_account_id IS 'Provider account selected for this provider checkout/session.';


--
-- Name: custom_credit_types; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.custom_credit_types (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    decimals integer DEFAULT 0 NOT NULL,
    active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT custom_credit_types_decimals_check CHECK (((decimals >= 0) AND (decimals <= 18)))
);

ALTER TABLE ONLY openrails.custom_credit_types FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE custom_credit_types; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.custom_credit_types IS 'Per-tenant custom credit units (#475): consume-only, no FX, never billed in. Referenced from money rows via the qualified code tenant-slug/name.';


--
-- Name: COLUMN custom_credit_types.decimals; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.custom_credit_types.decimals IS 'Minor-unit scale for presentation (10^decimals minor units per major unit). Storage is always integer minor units.';


--
-- Name: customers; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.customers (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    org_id text,
    issuer text,
    subject text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.customers FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE customers; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.customers IS 'OpenRails payable identity. Customer identity is merchant_id plus the host/AuthKit stable UUID subject; id is that payable UUID. issuer is audit/last-seen source only.';


--
-- Name: COLUMN customers.org_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.customers.org_id IS 'Deprecated customer identity metadata. Merchant ownership is represented by merchant_id; org/issuer do not key customers.';


--
-- Name: COLUMN customers.issuer; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.customers.issuer IS 'Audit/last-seen source issuer for delegated/remote customer touches. Not part of customer identity.';


--
-- Name: COLUMN customers.subject; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.customers.subject IS 'Host/AuthKit stable UUID subject. Natural key is (merchant_id, subject); issuer does not participate.';


--
-- Name: entitlement_features; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.entitlement_features (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    lookup_key text NOT NULL,
    name text NOT NULL,
    metadata jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.entitlement_features FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE entitlement_features; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.entitlement_features IS 'Stripe-shaped first-class entitlement feature definitions (issue #245). lookup_key is the stable value carried in AuthKit JWT entitlements and host-app checks. The internal openrails.entitlements window ledger remains the source of truth for active access.';


--
-- Name: entitlements; Type: TABLE; Schema: openrails; Owner: -
--

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
    grant_id uuid,
    CONSTRAINT chk_entitlements_source_type CHECK ((source_type = ANY (ARRAY['subscription'::text, 'one_off'::text, 'admin'::text, 'grace'::text, 'grant'::text]))),
    CONSTRAINT chk_revoke_fields_together CHECK (((revoked_at IS NULL) = (revoke_reason IS NULL))),
    CONSTRAINT chk_valid_time_window CHECK (((end_at IS NULL) OR (start_at < end_at)))
);

ALTER TABLE ONLY openrails.entitlements FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN entitlements.customer_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.entitlements.customer_id IS 'OpenRails payable tenant subject for this entitlement window.';


--
-- Name: external_provider_mutation_logs; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.external_provider_mutation_logs (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    provider text NOT NULL,
    provider_account_id uuid,
    provider_intent_id uuid,
    intent_type text,
    idempotency_key text,
    attempt integer DEFAULT 0 NOT NULL,
    phase text NOT NULL,
    reason text,
    evidence jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT external_provider_mutation_logs_phase_check CHECK ((phase = ANY (ARRAY['attempting'::text, 'succeeded'::text, 'failed'::text, 'unknown'::text, 'parked'::text])))
);

ALTER TABLE ONLY openrails.external_provider_mutation_logs FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE external_provider_mutation_logs; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.external_provider_mutation_logs IS 'Append-only operator history for external provider mutations executed from provider intents/convergence (#533).';


--
-- Name: COLUMN external_provider_mutation_logs.phase; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.external_provider_mutation_logs.phase IS 'Provider mutation lifecycle phase: attempting before the remote call, then succeeded/failed/unknown/parked after the handler classifies the result.';


--
-- Name: COLUMN external_provider_mutation_logs.evidence; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.external_provider_mutation_logs.evidence IS 'Scrubbed structured metadata only. Never store API keys, authorization headers, card data, private keys, or unsanitized provider bodies.';


--
-- Name: grants; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.grants (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    product_id uuid,
    kind text NOT NULL,
    source_type text NOT NULL,
    source_id text DEFAULT ''::text NOT NULL,
    payment_id uuid,
    event text DEFAULT 'grant'::text NOT NULL,
    supersedes_id uuid,
    spec_snapshot jsonb,
    starts_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    ends_at timestamp with time zone,
    amount bigint,
    currency text,
    reason text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT grants_amount_positive CHECK (((amount IS NULL) OR (amount > 0))),
    CONSTRAINT grants_credit_amount CHECK (((kind <> 'credit'::text) OR ((amount IS NOT NULL) AND (currency IS NOT NULL)))),
    CONSTRAINT grants_event_check CHECK ((event = ANY (ARRAY['grant'::text, 'revoke'::text, 'expire'::text, 'supersede'::text, 'adjust'::text]))),
    CONSTRAINT grants_event_supersedes CHECK (((event = 'grant'::text) = (supersedes_id IS NULL))),
    CONSTRAINT grants_kind_check CHECK ((kind = ANY (ARRAY['entitlement'::text, 'ownership'::text, 'credit'::text]))),
    CONSTRAINT grants_source_type_check CHECK ((source_type = ANY (ARRAY['purchase'::text, 'subscription'::text, 'admin'::text, 'grace'::text]))),
    CONSTRAINT grants_valid_window CHECK (((ends_at IS NULL) OR (starts_at < ends_at)))
);

ALTER TABLE ONLY openrails.grants FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE grants; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.grants IS '#514 append-only grant ledger: the access-domain sibling of the #512 money ledger. Immutable events (grant/revoke/expire/supersede/adjust); the live entitlement windows, product ownership, and credit lots are DERIVED projections folded from this log. A credit grant carries the lot amount+currency and IS the FIFO credit lot (subsumes the old money_blocks role); derive-2 emits its #512 deposit transfer tagged source=grant.';


--
-- Name: COLUMN grants.event; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.grants.event IS 'grant roots a grant; revoke/expire/supersede/adjust are new rows referencing it via supersedes_id. The grant row is never updated.';


--
-- Name: COLUMN grants.spec_snapshot; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.grants.spec_snapshot IS 'Product entitlements/credits spec captured at issuance so derive-2 (grant->projection) is a pure function and replay is exact + historical.';


--
-- Name: invoice_items; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.invoice_items (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
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
    CONSTRAINT invoice_items_amount_nonneg_chk CHECK ((amount >= 0)),
    CONSTRAINT invoice_items_quantity_positive_chk CHECK ((quantity > 0)),
    CONSTRAINT invoice_items_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'invoiced'::text, 'voided'::text])))
);

ALTER TABLE ONLY openrails.invoice_items FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE invoice_items; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.invoice_items IS 'Pending and invoiced billable items. Arrears usage creates pending items; invoice creation attaches them to a draft/open invoice.';


--
-- Name: invoice_payments; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.invoice_payments (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    invoice_id uuid NOT NULL,
    money_transaction_id uuid,
    currency text NOT NULL,
    amount bigint NOT NULL,
    status text DEFAULT 'attempted'::text NOT NULL,
    rail text,
    rail_payment_id text,
    failure_code text,
    failure_message text,
    attempted_at timestamp with time zone DEFAULT now() NOT NULL,
    settled_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    provider_account_id uuid,
    CONSTRAINT invoice_payments_amount_positive_chk CHECK ((amount > 0)),
    CONSTRAINT invoice_payments_status_check CHECK ((status = ANY (ARRAY['attempted'::text, 'settled'::text, 'failed'::text])))
);

ALTER TABLE ONLY openrails.invoice_payments FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE invoice_payments; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.invoice_payments IS 'Payment attempts and settled payments allocated to a specific invoice.';


--
-- Name: COLUMN invoice_payments.provider_account_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.invoice_payments.provider_account_id IS 'Provider account used for this invoice payment attempt or settled provider payment.';


--
-- Name: invoices; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.invoices (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
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
    CONSTRAINT invoices_amounts_nonneg_chk CHECK (((subtotal_amount >= 0) AND (total_amount >= 0) AND (amount_paid >= 0) AND (amount_due >= 0))),
    CONSTRAINT invoices_collection_method_check CHECK ((collection_method = ANY (ARRAY['charge_automatically'::text, 'send_invoice'::text]))),
    CONSTRAINT invoices_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'open'::text, 'paid'::text, 'past_due'::text, 'voided'::text, 'uncollectible'::text, 'finalized'::text])))
);

ALTER TABLE ONLY openrails.invoices FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE invoices; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.invoices IS 'Period invoices/statements. For arrears, an open invoice is the receivable and payments are allocated to it. Prepaid invoices remain informational receipts/statements.';


--
-- Name: COLUMN invoices.amount_due; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.invoices.amount_due IS 'Outstanding amount for this invoice in the row currency internal precision. Open arrears balance is derived from open/past-due invoices.';


--
-- Name: invoker_spend_limits; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.invoker_spend_limits (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    scope text NOT NULL,
    scope_key text DEFAULT ''::text NOT NULL,
    windows jsonb DEFAULT '[]'::jsonb NOT NULL,
    policy_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT invoker_spend_limits_scope_check CHECK ((scope = ANY (ARRAY['invoker'::text, 'role'::text, 'invoker_tier'::text])))
);

ALTER TABLE ONLY openrails.invoker_spend_limits FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE invoker_spend_limits; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.invoker_spend_limits IS 'Per-invoker spend limits (#473/#517): the payer caps how much a delegated invoker/role can spend of the payer''s money. {scope, scope_key, windows[]} composed in one admit verdict over the payer balance. Payer-set only.';


--
-- Name: COLUMN invoker_spend_limits.scope_key; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.invoker_spend_limits.scope_key IS 'Immutable scope discriminator: role uuid (scope=role), invoker string (scope=invoker), or tier key (scope=invoker_tier).';


--
-- Name: ledger_accounts; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.ledger_accounts (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid,
    account_type text NOT NULL,
    currency text NOT NULL,
    debits_must_not_exceed_credits boolean DEFAULT false NOT NULL,
    credits_must_not_exceed_debits boolean DEFAULT false NOT NULL,
    credits_posted bigint DEFAULT 0 NOT NULL,
    debits_posted bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT ledger_accounts_type_check CHECK ((account_type = ANY (ARRAY['customer_balance'::text, 'platform_revenue'::text, 'processor_clearing'::text, 'arrears_liability'::text, 'expired_credits'::text, 'revoked_credits'::text, 'fx_liquidity'::text, 'world'::text])))
);

ALTER TABLE ONLY openrails.ledger_accounts FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE ledger_accounts; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.ledger_accounts IS '#512 double-entry ledger accounts. One account belongs to exactly one (merchant, currency) ledger; TB-style posted/pending counters are maintained from immutable ledger_transfers and verified by reconciliation. account_type identifies its role (customer_balance, platform_revenue, processor_clearing, arrears_liability, expired_credits, fx_liquidity, world).';


--
-- Name: COLUMN ledger_accounts.customer_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.ledger_accounts.customer_id IS 'NULL for system accounts (one per merchant+currency); set for per-customer balance accounts.';


--
-- Name: COLUMN ledger_accounts.debits_must_not_exceed_credits; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.ledger_accounts.debits_must_not_exceed_credits IS 'TB sign flag: balance (credits-debits) may not go below zero (minus an applier-supplied arrears floor). Set on customer_balance.';


--
-- Name: COLUMN ledger_accounts.credits_posted; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.ledger_accounts.credits_posted IS 'Phase H maintained counter: posted credits for O(1) balance reads.';


--
-- Name: COLUMN ledger_accounts.debits_posted; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.ledger_accounts.debits_posted IS 'Phase H maintained counter: posted debits for O(1) balance reads.';


--
-- Name: ledger_transfers; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.ledger_transfers (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    debit_account_id uuid NOT NULL,
    credit_account_id uuid NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    transfer_type text NOT NULL,
    allow_debit_negative_up_to bigint DEFAULT 0 NOT NULL,
    source text,
    source_id text,
    grant_id uuid,
    customer_id uuid,
    invoker_id text,
    resource text,
    invoice_id uuid,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT ledger_transfers_amount_positive CHECK ((amount > 0)),
    CONSTRAINT ledger_transfers_debit_floor_nonnegative CHECK ((allow_debit_negative_up_to >= 0)),
    CONSTRAINT ledger_transfers_distinct_accounts CHECK ((debit_account_id <> credit_account_id))
);

ALTER TABLE ONLY openrails.ledger_transfers FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE ledger_transfers; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.ledger_transfers IS '#512 immutable double-entry transfers. Append-only (role granted SELECT,INSERT only). A transfer moves amount debit->credit within ONE (merchant, currency) ledger; capture/void/refund/expiry are NEW rows, never updates. ledger_accounts counters are a maintained projection of this table.';


--
-- Name: COLUMN ledger_transfers.allow_debit_negative_up_to; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.ledger_transfers.allow_debit_negative_up_to IS 'Debit-account floor used by the counter trigger for debits_must_not_exceed_credits accounts. Usually 0; arrears paths pass the current credit-line allowance.';


--
-- Name: COLUMN ledger_transfers.source; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.ledger_transfers.source IS 'Opaque origin key (e.g. ''grant''/grant_id, ''payment''/transaction_id). Ledger purity: business joins live in control-plane tables.';


--
-- Name: merchant_configurations; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.merchant_configurations (
    merchant_id uuid NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    config_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.merchant_configurations FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE merchant_configurations; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.merchant_configurations IS 'One merchant-scoped JSON configuration row. Missing keys use service defaults.';


--
-- Name: COLUMN merchant_configurations.config; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.merchant_configurations.config IS 'JSONB merchant config. delegated_invoker_wasted_spend_windows is an array of {key, window_seconds, limit}; amount values use the request currency internal precision.';


--
-- Name: merchant_credential_audit; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.merchant_credential_audit (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    action text NOT NULL,
    actor text,
    detail text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT merchant_credential_audit_action_check CHECK ((action = ANY (ARRAY['put'::text, 'rotate'::text, 'delete'::text, 'test'::text])))
);

ALTER TABLE ONLY openrails.merchant_credential_audit FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE merchant_credential_audit; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.merchant_credential_audit IS 'Append-only audit log of per-merchant credential put/rotate/delete/test events (issue #225). Merchant-owned and RLS protected.';


--
-- Name: merchant_deks; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.merchant_deks (
    merchant_id uuid NOT NULL,
    wrapped_dek bytea NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

ALTER TABLE ONLY openrails.merchant_deks FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE merchant_deks; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.merchant_deks IS 'Wrapped per-merchant Data Encryption Keys for envelope encryption-at-rest (issue #227). wrapped_dek = merchant DEK sealed with the master key (AES-256-GCM, nonce||ct||tag). Master key lives in config/env (self-hosted) or KMS (production), never in the DB. Merchant-owned and RLS protected.';


--
-- Name: COLUMN merchant_deks.wrapped_dek; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.merchant_deks.wrapped_dek IS 'AES-256-GCM(master_key, merchant_dek): nonce(12) || ciphertext(32) || tag(16).';


--
-- Name: merchant_exports; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.merchant_exports (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    status text DEFAULT 'completed'::text NOT NULL,
    location text,
    row_counts jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT merchant_exports_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'completed'::text, 'failed'::text])))
);

ALTER TABLE ONLY openrails.merchant_exports FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE merchant_exports; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.merchant_exports IS 'Merchant logical-export bookkeeping (issue #225). Merchant deletion is gated on a completed export row (export-before-delete). Merchant-owned and RLS protected.';


--
-- Name: merchant_secrets; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.merchant_secrets (
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    value text NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

ALTER TABLE ONLY openrails.merchant_secrets FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE merchant_secrets; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.merchant_secrets IS 'DB-backed per-merchant secret store (issue #225). Namespaced by (merchant_id, name). The Vault-backed store keeps the same addressing but holds values in Vault. Merchant-owned and RLS protected.';


--
-- Name: merchants; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.merchants (
    id uuid DEFAULT uuidv7() NOT NULL,
    slug text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    permission_group_id text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    deleted_at timestamp with time zone,
    CONSTRAINT merchants_status_check CHECK ((status = ANY (ARRAY['active'::text, 'deleted'::text])))
);


--
-- Name: TABLE merchants; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.merchants IS 'Merchant / billing-namespace directory: a dumb billing bucket (whose books a row goes on). GLOBAL (control-plane) table, not tenant-scoped. Carries ONLY billing/money-rail state, NO auth. Merchants are registered explicitly; there is no default merchant.';


--
-- Name: COLUMN merchants.slug; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.merchants.slug IS 'Stable merchant slug used in merchant-scoped routes and resolution.';


--
-- Name: COLUMN merchants.permission_group_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.merchants.permission_group_id IS 'The merchant''s OWN authkit permission-group id (#567 — a merchant is a top-level `merchant` group, child of `root`, with no parent org; supersedes #527''s owner_org_id 1:1 coupling). Bare `text`, NO FK into the auth schema (#544 portability guard). NULL in embedded (no control plane). Used to resolve a merchant from its authenticated group id.';


--
-- Name: money_settings; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.money_settings (
    id uuid NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
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
    CONSTRAINT money_settings_alert_pct_chk CHECK (((alert_threshold_pct >= 0) AND (alert_threshold_pct <= 100))),
    CONSTRAINT money_settings_billing_mode_chk CHECK ((billing_mode = ANY (ARRAY['prepaid'::text, 'arrears'::text]))),
    CONSTRAINT money_settings_credit_limit_amount_nonneg_chk CHECK ((credit_limit_amount >= 0)),
    CONSTRAINT money_settings_outstanding_owed_nonneg_chk CHECK ((outstanding_owed_amount >= 0)),
    CONSTRAINT money_settings_tier_source_chk CHECK ((tier_source = ANY (ARRAY['auto'::text, 'admin'::text])))
);

ALTER TABLE ONLY openrails.money_settings FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE money_settings; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.money_settings IS 'Per-(merchant, customer, currency) spend policy and money-in config. Amount values use the row currency internal precision.';


--
-- Name: COLUMN money_settings.max_spend_per_day; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.money_settings.max_spend_per_day IS 'Optional daily spend cap in the row currency internal precision; NULL = uncapped.';


--
-- Name: COLUMN money_settings.max_spend_per_month; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.money_settings.max_spend_per_month IS 'Optional monthly spend cap in the row currency internal precision; NULL = uncapped.';


--
-- Name: COLUMN money_settings.max_outstanding_owed_amount; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.money_settings.max_outstanding_owed_amount IS 'Optional arrears owed ceiling in the row currency internal precision; NULL = uncapped.';


--
-- Name: COLUMN money_settings.low_balance_threshold; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.money_settings.low_balance_threshold IS 'Optional low-balance trigger in the row currency internal precision.';


--
-- Name: COLUMN money_settings.outstanding_owed_amount; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.money_settings.outstanding_owed_amount IS 'Current arrears owed amount in the row currency internal precision.';


--
-- Name: COLUMN money_settings.verified_payment_method; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.money_settings.verified_payment_method IS 'Legacy metadata noting that a collection method was verified; service admission consumes computed credit capacity instead of checking this flag.';


--
-- Name: COLUMN money_settings.suspended_at; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.money_settings.suspended_at IS 'Legacy account-freeze metadata; service admission does not consult this flag.';


--
-- Name: COLUMN money_settings.tier_source; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.money_settings.tier_source IS 'auto = tier maintained by tier_schedule auto-graduation; admin = explicit override that auto-graduation must not overwrite.';


--
-- Name: COLUMN money_settings.currency; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.money_settings.currency IS 'System currency code (USD/EUR/JPY); the Go registry is the authority. Stablecoins and crypto tokens are payment assets, not account currencies.';


--
-- Name: COLUMN money_settings.credit_limit_amount; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.money_settings.credit_limit_amount IS 'Admin-set arrears credit line in the row currency internal precision. 0 = no arrears capacity; prepaid balance may still be spent.';


--
-- Name: notification_queue; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.notification_queue (
    id uuid DEFAULT uuidv7() NOT NULL,
    event_type text NOT NULL,
    data jsonb NOT NULL,
    seen boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL
);

ALTER TABLE ONLY openrails.notification_queue FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE notification_queue; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.notification_queue IS 'Queue for user notifications related to billing and subscriptions';


--
-- Name: payer_spend_limits; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.payer_spend_limits (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid,
    tier text NOT NULL,
    policy jsonb DEFAULT '{}'::jsonb NOT NULL,
    policy_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.payer_spend_limits FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE payer_spend_limits; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.payer_spend_limits IS 'Per-tier payer spend limit (#477/#517): the platform caps the payer''s spend, keyed by trust-tier. customer_id NULL is the merchant-wide default; non-NULL is a per-customer override.';


--
-- Name: COLUMN payer_spend_limits.customer_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.payer_spend_limits.customer_id IS 'NULL = merchant-wide default tier limit (#477); non-NULL = per-customer override taking precedence for that customer.';


--
-- Name: COLUMN payer_spend_limits.policy; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.payer_spend_limits.policy IS 'JSONB tier money policy: budget_windows and bad_spend_windows. Money values use the request currency internal precision.';


--
-- Name: payment_blocklist; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.payment_blocklist (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid,
    kind text NOT NULL,
    value text NOT NULL,
    reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT payment_blocklist_kind_check CHECK ((kind = ANY (ARRAY['card_fingerprint'::text, 'processor_customer'::text, 'email'::text, 'ip'::text])))
);

ALTER TABLE ONLY openrails.payment_blocklist FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE payment_blocklist; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.payment_blocklist IS 'Tenant-scoped blocklist of known-bad payment identifiers (issue #300). customer_id NULL = tenant-wide block; set = tenant-subject scoped. Checkout/admission deny wiring is a separate slice.';


--
-- Name: payment_methods; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.payment_methods (
    id uuid DEFAULT uuidv7() NOT NULL,
    rail character varying(50) NOT NULL,
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
    provider_account_id uuid
);

ALTER TABLE ONLY openrails.payment_methods FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE payment_methods; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.payment_methods IS 'Generalized payment method table supporting multiple rails.';


--
-- Name: COLUMN payment_methods.rail; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.payment_methods.rail IS 'Payment rail type: nmi, ccbill, stripe, etc.';


--
-- Name: COLUMN payment_methods.vault_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.payment_methods.vault_id IS 'Primary payment method identifier in the rail system';


--
-- Name: COLUMN payment_methods.provider_account_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.payment_methods.provider_account_id IS 'Provider account that produced this vaulted payment method mirror row.';


--
-- Name: payments; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.payments (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid NOT NULL,
    rail openrails.rail_type NOT NULL,
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
    provider_account_id uuid,
    CONSTRAINT chk_payment_not_future CHECK ((purchased_at <= (now() + '00:05:00'::interval)))
);

ALTER TABLE ONLY openrails.payments FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE payments; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.payments IS 'Records of all payment transactions (formerly purchases table)';


--
-- Name: COLUMN payments.subscription_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.payments.subscription_id IS 'Links a payment to the subscription that generated it (nullable for one-off payments)';


--
-- Name: COLUMN payments.provider_account_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.payments.provider_account_id IS 'Provider account that produced this payment/charge mirror row.';


--
-- Name: prices; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.prices (
    id uuid DEFAULT uuidv7() NOT NULL,
    product_id uuid NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    billing_cycle_days integer,
    rails jsonb,
    status text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    CONSTRAINT prices_amount_nonneg_chk CHECK ((amount >= 0)),
    CONSTRAINT prices_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'active'::text, 'archived'::text])))
);

ALTER TABLE ONLY openrails.prices FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE prices; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.prices IS 'Pricing tiers for products with rail-specific identifiers';


--
-- Name: probe_verdicts; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.probe_verdicts (
    provider text NOT NULL,
    key_hash text NOT NULL,
    verdict text NOT NULL,
    checked_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT chk_probe_verdicts_verdict CHECK ((verdict = ANY (ARRAY['live'::text, 'simulated'::text])))
);


--
-- Name: TABLE probe_verdicts; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.probe_verdicts IS 'Cached NMI test-mode probe verdicts (#348): one row per (provider, sha256(security_key)). Fresh ''live'' refuses boot from cache, fresh ''simulated'' skips the probe, stale/missing re-probes. RLS-exempt by design: instance-level credential state, not tenant data.';


--
-- Name: COLUMN probe_verdicts.key_hash; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.probe_verdicts.key_hash IS 'sha256 hex of the provider security key. A rotated key hashes differently, so the cache never answers for a credential it has not seen.';


--
-- Name: rail_customers; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.rail_customers (
    id uuid DEFAULT uuidv7() NOT NULL,
    rail text NOT NULL,
    rail_customer_id text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    provider_account_id uuid
);

ALTER TABLE ONLY openrails.rail_customers FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN rail_customers.provider_account_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.rail_customers.provider_account_id IS 'Provider account that produced this rail customer mirror row.';


--
-- Name: product_entitlement_features; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.product_entitlement_features (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    entitlement_feature_id uuid NOT NULL,
    duration_days integer,
    metadata jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.product_entitlement_features FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE product_entitlement_features; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.product_entitlement_features IS 'Stripe-shaped product_feature attachments (issue #245): which entitlement features a product grants when purchased. duration_days null = indefinite.';


--
-- Name: products; Type: TABLE; Schema: openrails; Owner: -
--

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
    CONSTRAINT products_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'active'::text, 'archived'::text])))
);

ALTER TABLE ONLY openrails.products FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE products; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.products IS 'Product definitions that can be purchased or subscribed to';


--
-- Name: COLUMN products.credits_spec; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.products.credits_spec IS 'Bundled promo credits spec (amount, expiry, cadence) for subscriptions';


--
-- Name: COLUMN products.tier_group; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.products.tier_group IS 'Semantic group name for mutually-exclusive products (e.g., "premium"). Products in same group require upgrade/downgrade, not parallel ownership.';


--
-- Name: COLUMN products.tier_rank; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.products.tier_rank IS 'Tier ranking within group. Higher = more premium. Used to determine upgrade (higher rank) vs downgrade (lower rank) direction.';


--
-- Name: provider_accounts; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.provider_accounts (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    provider_type text NOT NULL,
    environment text DEFAULT 'live'::text NOT NULL,
    account_id text NOT NULL,
    display_name text,
    vault_secret_ref text,
    role text DEFAULT 'primary'::text NOT NULL,
    status text DEFAULT 'enabled'::text NOT NULL,
    evidence jsonb,
    first_seen_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    last_verified_at timestamp with time zone,
    replaced_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT provider_accounts_environment_check CHECK ((environment = ANY (ARRAY['live'::text, 'test'::text]))),
    CONSTRAINT provider_accounts_nonempty CHECK (((btrim(provider_type) <> ''::text) AND (btrim(environment) <> ''::text) AND (btrim(account_id) <> ''::text))),
    CONSTRAINT provider_accounts_role_check CHECK ((role = ANY (ARRAY['primary'::text, 'secondary'::text, 'legacy'::text]))),
    CONSTRAINT provider_accounts_status_check CHECK ((status = ANY (ARRAY['enabled'::text, 'disabled'::text])))
);

ALTER TABLE ONLY openrails.provider_accounts FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE provider_accounts; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.provider_accounts IS 'Merchant-scoped provider account registry (#518). account_id is the provider-returned account/profile identity, not a credential hash.';


--
-- Name: COLUMN provider_accounts.provider_type; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_accounts.provider_type IS 'Provider rail/type such as stripe, nmi, ccbill, solana, or a future provider type.';


--
-- Name: COLUMN provider_accounts.environment; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_accounts.environment IS 'Provider environment: live or test. Live and test accounts are distinct identities and may each have their own primary.';


--
-- Name: COLUMN provider_accounts.account_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_accounts.account_id IS 'Provider-returned account identity, e.g. Stripe acct_..., NMI profile account id, CCBill account/subaccount, or Solana authority address.';


--
-- Name: COLUMN provider_accounts.role; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_accounts.role IS 'primary routes new work by default; secondary is enabled but explicit/manual; legacy is for old rows/rebills/refunds/webhooks only.';


--
-- Name: COLUMN provider_accounts.status; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_accounts.status IS 'enabled participates in routing/reconcile; disabled is retained for history but should not receive new routine work.';


--
-- Name: provider_intents; Type: TABLE; Schema: openrails; Owner: -
--

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
    provider_account_id uuid,
    CONSTRAINT chk_provider_intents_executed CHECK (((status <> 'succeeded'::text) OR (executed_at IS NOT NULL))),
    CONSTRAINT chk_provider_intents_origin CHECK ((origin = ANY (ARRAY['user'::text, 'admin'::text, 'system'::text]))),
    CONSTRAINT chk_provider_intents_status CHECK ((status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'succeeded'::text, 'unknown_needs_verify'::text, 'failed_retryable'::text, 'failed_terminal'::text, 'superseded'::text, 'expired'::text])))
);

ALTER TABLE ONLY openrails.provider_intents FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE provider_intents; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.provider_intents IS 'Durable, effectively-once outbox for outbound provider mutations (#358). One row per logical intent (unique per tenant on idempotency_key); the executor worker drains whatever is currently executable, the verifier resolves ambiguous outcomes via provider reads.';


--
-- Name: COLUMN provider_intents.provider; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_intents.provider IS 'Provider/rail key the mutation targets (e.g. ''mobius'' for an NMI-backed rail, ''stripe'').';


--
-- Name: COLUMN provider_intents.intent_type; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_intents.intent_type IS 'Registry key selecting the per-type semantics (executor, verifier, relevance, backoff): nmi_delete_subscription, and in later phases manual_rebill, refund, plan_archive, ...';


--
-- Name: COLUMN provider_intents.idempotency_key; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_intents.idempotency_key IS 'Deterministic identity of the logical intent within the tenant. Re-enqueues conflict here: a pending intent is refreshed, a superseded/expired one revived (relevance returned), anything else untouched — effectively-once per logical intent.';


--
-- Name: COLUMN provider_intents.claimed_until; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_intents.claimed_until IS 'Single-executor lease (SKIP LOCKED claim). An in_flight row whose lease elapsed was orphaned by a crashed executor and becomes claimable again; per-type execute semantics (verify-then-execute, verifier-before-retry) make the reclaim safe.';


--
-- Name: COLUMN provider_intents.origin; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_intents.origin IS 'Who wanted this mutation: user/admin-origin intents execute under mode=limited (reactive completion), system-origin intents require mode=full. Nothing executes under mode=readonly.';


--
-- Name: COLUMN provider_intents.last_failure_reason; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_intents.last_failure_reason IS 'Why the most recent attempt did not succeed (mode parked, kill switch, provider down, declined...). Recorded on the intent, never surfaced as an error.';


--
-- Name: COLUMN provider_intents.expires_at; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_intents.expires_at IS 'End of the relevance window: past this instant the intent expires with a finding instead of firing stale (NULL = relevance governed solely by the type''s relevance check).';


--
-- Name: COLUMN provider_intents.result_evidence; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_intents.result_evidence IS 'How the terminal status was established (e.g. {"verified_absent": true} for a delete confirmed by a provider read).';


--
-- Name: COLUMN provider_intents.provider_account_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_intents.provider_account_id IS 'Provider account row the outbound intent was enqueued against. Mismatch with current credentials parks/defers execution.';


--
-- Name: provider_refresh_watermarks; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.provider_refresh_watermarks (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    provider text NOT NULL,
    provider_account_id uuid,
    provider_account_key uuid GENERATED ALWAYS AS (COALESCE(provider_account_id, '00000000-0000-0000-0000-000000000000'::uuid)) STORED,
    event_domain text NOT NULL,
    watermark_at timestamp with time zone NOT NULL,
    last_attempted_at timestamp with time zone,
    last_succeeded_at timestamp with time zone,
    last_error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT provider_refresh_watermarks_account_nonzero CHECK (((provider_account_id IS NULL) OR (provider_account_id <> '00000000-0000-0000-0000-000000000000'::uuid))),
    CONSTRAINT provider_refresh_watermarks_event_domain_check CHECK ((event_domain = ANY (ARRAY['events'::text]))),
    CONSTRAINT provider_refresh_watermarks_provider_check CHECK ((provider = ANY (ARRAY['nmi'::text, 'ccbill'::text, 'stripe'::text, 'solana'::text])))
);

ALTER TABLE ONLY openrails.provider_refresh_watermarks FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE provider_refresh_watermarks; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.provider_refresh_watermarks IS 'Durable Provider Refresh watermarks. A failed or partial provider read records last_error but never advances watermark_at.';


--
-- Name: COLUMN provider_refresh_watermarks.provider_account_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_refresh_watermarks.provider_account_id IS 'Current provider account row when resolvable; NULL is the compatibility/global lane for providers without a bound account identity.';


--
-- Name: COLUMN provider_refresh_watermarks.event_domain; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_refresh_watermarks.event_domain IS 'Refresh domain. events currently covers provider transaction/subscription event windows.';


--
-- Name: COLUMN provider_refresh_watermarks.watermark_at; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.provider_refresh_watermarks.watermark_at IS 'Exclusive lower bound for the next successful bounded provider event refresh window.';


--
-- Name: reconciliation_findings; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.reconciliation_findings (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    finding_type text NOT NULL,
    subject_key text NOT NULL,
    severity text NOT NULL,
    status text DEFAULT 'open'::text NOT NULL,
    recommended_action text,
    first_seen_run uuid NOT NULL,
    last_seen_run uuid NOT NULL,
    last_seen_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    resolved_at timestamp with time zone,
    resolution text,
    operator_notes text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    evidence jsonb,
    CONSTRAINT chk_reconciliation_findings_resolution CHECK (((resolution IS NULL) OR (resolution = ANY (ARRAY['auto_vanished'::text, 'enforced'::text, 'admin_fixed'::text, 'ignored'::text])))),
    CONSTRAINT chk_reconciliation_findings_resolved_fields CHECK ((((status = ANY (ARRAY['auto_fixed'::text, 'fixed'::text, 'ignored'::text])) AND (resolved_at IS NOT NULL) AND (resolution IS NOT NULL)) OR ((status = ANY (ARRAY['reconcile_required'::text, 'requires_review'::text])) AND (resolved_at IS NULL) AND (resolution IS NULL)))),
    CONSTRAINT chk_reconciliation_findings_severity CHECK ((severity = ANY (ARRAY['critical'::text, 'high'::text, 'medium'::text, 'low'::text]))),
    CONSTRAINT chk_reconciliation_findings_status CHECK ((status = ANY (ARRAY['auto_fixed'::text, 'reconcile_required'::text, 'requires_review'::text, 'fixed'::text, 'ignored'::text]))),
    CONSTRAINT chk_reconciliation_findings_type CHECK ((finding_type ~ '^(pull|derive|life|consistency)\.[a-z0-9_]+(\.[a-z0-9_]+)?$'::text))
);

ALTER TABLE ONLY openrails.reconciliation_findings FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE reconciliation_findings; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.reconciliation_findings IS 'Durable reconciliation findings ledger. Stable identity per (merchant, finding_type, subject_key); provider/account context lives in evidence for pull.* findings. Statuses: reconcile_required, requires_review, auto_fixed, fixed, ignored (#573).';


--
-- Name: COLUMN reconciliation_findings.subject_key; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.reconciliation_findings.subject_key IS 'Stable identity of the drifted subject within (provider, finding_type): rail subscription id, transaction id, local subscription/payment-method uuid, or tenant_subject uuid depending on the check.';


--
-- Name: COLUMN reconciliation_findings.operator_notes; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.reconciliation_findings.operator_notes IS 'Operator-entered notes attached when a finding is fixed or ignored manually.';


--
-- Name: COLUMN reconciliation_findings.evidence; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.reconciliation_findings.evidence IS 'Machine-readable finding evidence. Optional nested keys: provider, local, remote, intent, resolution.';


--
-- Name: reconciliation_runs; Type: TABLE; Schema: openrails; Owner: -
--

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
    CONSTRAINT chk_reconciliation_runs_mode CHECK ((mode = ANY (ARRAY['advisory'::text, 'enforce'::text]))),
    CONSTRAINT chk_reconciliation_runs_status CHECK ((status = ANY (ARRAY['running'::text, 'completed'::text, 'failed'::text])))
);

ALTER TABLE ONLY openrails.reconciliation_runs FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE reconciliation_runs; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.reconciliation_runs IS 'One row per manual reconcile run (#107): advisory diffs or enforce convergence against the payment rails. Summary jsonb carries per-provider counts and the dunning-forensics report.';


--
-- Name: reconciliation_state; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.reconciliation_state (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    source_domain text NOT NULL,
    fully_reconciled boolean DEFAULT false NOT NULL,
    last_full_pull_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_reconciliation_state_domain CHECK ((source_domain = ANY (ARRAY['subscriptions'::text, 'payments'::text, 'grants'::text])))
);

ALTER TABLE ONLY openrails.reconciliation_state FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE reconciliation_state; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.reconciliation_state IS '#511 per-(merchant, source_domain) reconciliation watermark. fully_reconciled gates the confirmed-absence rule: a destructive EXCESS repair is HELD until its source domain (subscriptions|payments|grants) is proven fully reconciled.';


--
-- Name: solana_subscriptions; Type: TABLE; Schema: openrails; Owner: -
--

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
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.solana_subscriptions FORCE ROW LEVEL SECURITY;


--
-- Name: subscriptions; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.subscriptions (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid,
    product_id uuid NOT NULL,
    status openrails.subscription_status DEFAULT 'pending'::openrails.subscription_status NOT NULL,
    rail text DEFAULT 'ccbill'::text NOT NULL,
    rail_subscription_id text DEFAULT ''::text NOT NULL,
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
    provider_account_id uuid,
    CONSTRAINT chk_cancelled_has_timestamp CHECK (((status <> 'cancelled'::openrails.subscription_status) OR (cancelled_at IS NOT NULL))),
    CONSTRAINT chk_cancelled_has_type CHECK (((status <> 'cancelled'::openrails.subscription_status) OR (cancel_type IS NOT NULL))),
    CONSTRAINT chk_cancelled_no_retry_schedule CHECK (((status <> 'cancelled'::openrails.subscription_status) OR ((next_retry_at IS NULL) AND (grace_ends_at IS NULL)))),
    CONSTRAINT chk_ended_not_before_cancelled CHECK (((ended_at IS NULL) OR (cancelled_at IS NULL) OR (ended_at >= cancelled_at))),
    CONSTRAINT chk_past_due_has_period_end CHECK (((status <> 'past_due'::openrails.subscription_status) OR (current_period_ends_at IS NOT NULL))),
    CONSTRAINT chk_valid_period CHECK (((current_period_starts_at IS NULL) OR (current_period_ends_at IS NULL) OR (current_period_starts_at < current_period_ends_at)))
);

ALTER TABLE ONLY openrails.subscriptions FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE subscriptions; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.subscriptions IS 'Core subscription records tracking user billing relationships';


--
-- Name: COLUMN subscriptions.product_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.subscriptions.product_id IS 'Denormalized product ID for efficient user+product lookups without joining prices';


--
-- Name: COLUMN subscriptions.scheduled_price_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.subscriptions.scheduled_price_id IS 'Price ID for scheduled tier change (downgrade). Applied at end of current billing period during renewal.';


--
-- Name: COLUMN subscriptions.tier_group; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.subscriptions.tier_group IS 'Denormalized from openrails.products.tier_group (kept in sync by trigger trg_subscriptions_set_tier_group). Backs uq_subscriptions_user_tier_group_active, which enforces one active/pending subscription per (user, tier group).';


--
-- Name: COLUMN subscriptions.provider_account_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.subscriptions.provider_account_id IS 'Provider account that produced this remote subscription mirror row.';


--
-- Name: tier_schedules; Type: TABLE; Schema: openrails; Owner: -
--

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
    CONSTRAINT tier_schedules_owner_check CHECK ((owner = ANY (ARRAY['platform'::text, 'subject'::text])))
);

ALTER TABLE ONLY openrails.tier_schedules FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE tier_schedules; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.tier_schedules IS 'Persisted tier ladder (#476): rungs declared once per merchant and currency, or as a per-customer/currency override. OpenRails auto-maintains money_settings.tier from same-currency cumulative paid spend unless tier_source=admin.';


--
-- Name: COLUMN tier_schedules.customer_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.tier_schedules.customer_id IS 'NULL = merchant-wide default schedule for this currency; non-NULL = per-customer override taking precedence for that customer/currency.';


--
-- Name: COLUMN tier_schedules.currency; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.tier_schedules.currency IS 'Currency whose cumulative paid amount is compared to this ladder.';


--
-- Name: COLUMN tier_schedules.owner; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.tier_schedules.owner IS 'platform (set by us; subject cannot edit/see) | subject.';


--
-- Name: COLUMN tier_schedules.rungs; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.tier_schedules.rungs IS 'Ordered JSONB array of {tier, min_cumulative_paid_amount}; a payer''s tier = highest rung whose min_cumulative_paid_amount <= same-currency cumulative_paid.';


--
-- Name: usage_events; Type: TABLE; Schema: openrails; Owner: -
--

CREATE TABLE openrails.usage_events (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    invoker_id text NOT NULL,
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
    CONSTRAINT usage_events_amount_check CHECK ((amount >= 0))
);

ALTER TABLE ONLY openrails.usage_events FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE usage_events; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.usage_events IS 'Append-only multi-dimensional metered usage (issue #289). Source of truth for usage reporting + #303 invoice line items. Host-priced (amount sent by the host); event + ledger debit commit in one tx. The hot admission path (#298) never reads this table.';


--
-- Name: COLUMN usage_events.invoker_id; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.usage_events.invoker_id IS 'Caller-supplied principal string that fired this metered usage event. Opaque to OpenRails; attribution + grouping only, not a FK. Joins use source/source_id.';


--
-- Name: COLUMN usage_events.currency; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.usage_events.currency IS 'Native OpenRails currency code; amount uses this currency internal precision.';


--
-- Name: COLUMN usage_events.resource; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON COLUMN openrails.usage_events.resource IS 'Caller-supplied free-form string for what was metered (tensorhub: endpoint slug; doujins: plan/item slug). Opaque to OpenRails; nullable, not a FK.';


--
-- Name: usdc_funding_sessions; Type: TABLE; Schema: openrails; Owner: -
--

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
    CONSTRAINT usdc_funding_sessions_asset_valid CHECK ((asset = 'USDC'::text)),
    CONSTRAINT usdc_funding_sessions_nonempty CHECK (((btrim(wallet_address) <> ''::text) AND (btrim(network) <> ''::text) AND (btrim(requested_amount) <> ''::text) AND (btrim(provider_url) <> ''::text))),
    CONSTRAINT usdc_funding_sessions_provider_valid CHECK ((provider = ANY (ARRAY['robinhood'::text, 'coinbase'::text]))),
    CONSTRAINT usdc_funding_sessions_status_valid CHECK ((status = ANY (ARRAY['created'::text, 'opened'::text, 'pending_provider'::text, 'pending_settlement'::text, 'funded'::text, 'failed'::text, 'expired'::text, 'cancelled'::text])))
);

ALTER TABLE ONLY openrails.usdc_funding_sessions FORCE ROW LEVEL SECURITY;


--
-- Name: TABLE usdc_funding_sessions; Type: COMMENT; Schema: openrails; Owner: -
--

COMMENT ON TABLE openrails.usdc_funding_sessions IS 'External Robinhood/Coinbase handoffs that fund USDC into a user self-custody wallet before normal OpenRails wallet checkout. Return from provider is not proof of funding.';


--
-- Name: bootstrap_state bootstrap_state_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.bootstrap_state
    ADD CONSTRAINT bootstrap_state_pkey PRIMARY KEY (singleton);


--
-- Name: catalog_drift_events catalog_drift_events_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.catalog_drift_events
    ADD CONSTRAINT catalog_drift_events_pkey PRIMARY KEY (id);


--
-- Name: checkout_sessions checkout_sessions_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_pkey PRIMARY KEY (id);


--
-- Name: custom_credit_types custom_credit_types_merchant_name_key; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.custom_credit_types
    ADD CONSTRAINT custom_credit_types_merchant_name_key UNIQUE (merchant_id, name);


--
-- Name: custom_credit_types custom_credit_types_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.custom_credit_types
    ADD CONSTRAINT custom_credit_types_pkey PRIMARY KEY (id);


--
-- Name: customers customers_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.customers
    ADD CONSTRAINT customers_pkey PRIMARY KEY (id);


--
-- Name: entitlement_features entitlement_features_merchant_lookup_key_key; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.entitlement_features
    ADD CONSTRAINT entitlement_features_merchant_lookup_key_key UNIQUE (merchant_id, lookup_key);


--
-- Name: entitlement_features entitlement_features_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.entitlement_features
    ADD CONSTRAINT entitlement_features_pkey PRIMARY KEY (id);


--
-- Name: entitlements entitlements_customer_no_overlap; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.entitlements
    ADD CONSTRAINT entitlements_customer_no_overlap EXCLUDE USING gist (merchant_id WITH =, customer_id WITH =, entitlement WITH =, period WITH &&) WHERE (((customer_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL)));


--
-- Name: entitlements entitlements_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.entitlements
    ADD CONSTRAINT entitlements_pkey PRIMARY KEY (id);


--
-- Name: external_provider_mutation_logs external_provider_mutation_logs_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.external_provider_mutation_logs
    ADD CONSTRAINT external_provider_mutation_logs_pkey PRIMARY KEY (id);


--
-- Name: grants grants_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_pkey PRIMARY KEY (id);


--
-- Name: invoice_items invoice_items_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoice_items
    ADD CONSTRAINT invoice_items_pkey PRIMARY KEY (id);


--
-- Name: invoice_payments invoice_payments_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoice_payments
    ADD CONSTRAINT invoice_payments_pkey PRIMARY KEY (id);


--
-- Name: invoices invoices_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoices
    ADD CONSTRAINT invoices_pkey PRIMARY KEY (id);


--
-- Name: invoker_spend_limits invoker_spend_limits_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoker_spend_limits
    ADD CONSTRAINT invoker_spend_limits_pkey PRIMARY KEY (id);


--
-- Name: invoker_spend_limits invoker_spend_limits_uniq; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoker_spend_limits
    ADD CONSTRAINT invoker_spend_limits_uniq UNIQUE (merchant_id, customer_id, scope, scope_key);


--
-- Name: ledger_accounts ledger_accounts_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.ledger_accounts
    ADD CONSTRAINT ledger_accounts_pkey PRIMARY KEY (id);


--
-- Name: ledger_transfers ledger_transfers_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.ledger_transfers
    ADD CONSTRAINT ledger_transfers_pkey PRIMARY KEY (id);


--
-- Name: merchant_configurations merchant_configurations_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.merchant_configurations
    ADD CONSTRAINT merchant_configurations_pkey PRIMARY KEY (merchant_id);


--
-- Name: merchant_credential_audit merchant_credential_audit_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.merchant_credential_audit
    ADD CONSTRAINT merchant_credential_audit_pkey PRIMARY KEY (id);


--
-- Name: merchant_exports merchant_exports_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.merchant_exports
    ADD CONSTRAINT merchant_exports_pkey PRIMARY KEY (id);


--
-- Name: merchants merchants_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.merchants
    ADD CONSTRAINT merchants_pkey PRIMARY KEY (id);


--
-- Name: money_settings money_settings_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.money_settings
    ADD CONSTRAINT money_settings_pkey PRIMARY KEY (id);


--
-- Name: notification_queue notification_queue_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.notification_queue
    ADD CONSTRAINT notification_queue_pkey PRIMARY KEY (id);


--
-- Name: payer_spend_limits payer_spend_limits_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payer_spend_limits
    ADD CONSTRAINT payer_spend_limits_pkey PRIMARY KEY (id);


--
-- Name: payment_blocklist payment_blocklist_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payment_blocklist
    ADD CONSTRAINT payment_blocklist_pkey PRIMARY KEY (id);


--
-- Name: payment_methods payment_methods_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payment_methods
    ADD CONSTRAINT payment_methods_pkey PRIMARY KEY (id);


--
-- Name: payments payments_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_pkey PRIMARY KEY (id);


--
-- Name: merchant_deks pk_merchant_deks; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.merchant_deks
    ADD CONSTRAINT pk_merchant_deks PRIMARY KEY (merchant_id);


--
-- Name: merchant_secrets pk_merchant_secrets; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.merchant_secrets
    ADD CONSTRAINT pk_merchant_secrets PRIMARY KEY (merchant_id, name);


--
-- Name: prices prices_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.prices
    ADD CONSTRAINT prices_pkey PRIMARY KEY (id);


--
-- Name: probe_verdicts probe_verdicts_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.probe_verdicts
    ADD CONSTRAINT probe_verdicts_pkey PRIMARY KEY (provider, key_hash);


--
-- Name: rail_customers rail_customers_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.rail_customers
    ADD CONSTRAINT rail_customers_pkey PRIMARY KEY (id);


--
-- Name: product_entitlement_features product_entitlement_features_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.product_entitlement_features
    ADD CONSTRAINT product_entitlement_features_pkey PRIMARY KEY (id);


--
-- Name: product_entitlement_features product_entitlement_features_unique; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.product_entitlement_features
    ADD CONSTRAINT product_entitlement_features_unique UNIQUE (merchant_id, product_id, entitlement_feature_id);


--
-- Name: products products_merchant_slug_key; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.products
    ADD CONSTRAINT products_merchant_slug_key UNIQUE (merchant_id, slug);


--
-- Name: products products_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.products
    ADD CONSTRAINT products_pkey PRIMARY KEY (id);


--
-- Name: provider_accounts provider_accounts_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.provider_accounts
    ADD CONSTRAINT provider_accounts_pkey PRIMARY KEY (id);


--
-- Name: provider_intents provider_intents_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.provider_intents
    ADD CONSTRAINT provider_intents_pkey PRIMARY KEY (id);


--
-- Name: provider_refresh_watermarks provider_refresh_watermarks_identity_key; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.provider_refresh_watermarks
    ADD CONSTRAINT provider_refresh_watermarks_identity_key UNIQUE (merchant_id, provider, provider_account_key, event_domain);


--
-- Name: provider_refresh_watermarks provider_refresh_watermarks_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.provider_refresh_watermarks
    ADD CONSTRAINT provider_refresh_watermarks_pkey PRIMARY KEY (id);


--
-- Name: reconciliation_findings reconciliation_findings_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.reconciliation_findings
    ADD CONSTRAINT reconciliation_findings_pkey PRIMARY KEY (id);


--
-- Name: reconciliation_runs reconciliation_runs_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_pkey PRIMARY KEY (id);


--
-- Name: reconciliation_state reconciliation_state_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.reconciliation_state
    ADD CONSTRAINT reconciliation_state_pkey PRIMARY KEY (id);


--
-- Name: solana_subscriptions solana_subscriptions_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_pkey PRIMARY KEY (id);


--
-- Name: solana_subscriptions solana_subscriptions_subscription_pda_key; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_subscription_pda_key UNIQUE (subscription_pda);


--
-- Name: subscriptions subscriptions_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_pkey PRIMARY KEY (id);


--
-- Name: tier_schedules tier_schedules_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.tier_schedules
    ADD CONSTRAINT tier_schedules_pkey PRIMARY KEY (id);


--
-- Name: prices unique_prices_product_amount_cycle; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.prices
    ADD CONSTRAINT unique_prices_product_amount_cycle UNIQUE (product_id, amount, currency, billing_cycle_days);


--
-- Name: merchants uq_merchants_slug; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.merchants
    ADD CONSTRAINT uq_merchants_slug UNIQUE (slug);


--
-- Name: usage_events usage_events_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.usage_events
    ADD CONSTRAINT usage_events_pkey PRIMARY KEY (id);


--
-- Name: usdc_funding_sessions usdc_funding_sessions_pkey; Type: CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.usdc_funding_sessions
    ADD CONSTRAINT usdc_funding_sessions_pkey PRIMARY KEY (id);


--
-- Name: checkout_sessions_expires_at_idx; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX checkout_sessions_expires_at_idx ON openrails.checkout_sessions USING btree (expires_at);


--
-- Name: checkout_sessions_rail_reference_idx; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX checkout_sessions_rail_reference_idx ON openrails.checkout_sessions USING btree (rail, reference) WHERE (reference IS NOT NULL);


--
-- Name: checkout_sessions_rail_transaction_id_idx; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX checkout_sessions_rail_transaction_id_idx ON openrails.checkout_sessions USING btree (rail, transaction_id) WHERE (transaction_id IS NOT NULL);


--
-- Name: idx_catalog_drift_events_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_catalog_drift_events_merchant_id ON openrails.catalog_drift_events USING btree (merchant_id);


--
-- Name: idx_catalog_drift_events_open; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_catalog_drift_events_open ON openrails.catalog_drift_events USING btree (detected_at DESC) WHERE (resolved_at IS NULL);


--
-- Name: idx_checkout_sessions_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_checkout_sessions_customer ON openrails.checkout_sessions USING btree (customer_id) WHERE (customer_id IS NOT NULL);


--
-- Name: idx_checkout_sessions_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_checkout_sessions_merchant_id ON openrails.checkout_sessions USING btree (merchant_id);


--
-- Name: idx_checkout_sessions_provider_account; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_checkout_sessions_provider_account ON openrails.checkout_sessions USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: idx_custom_credit_types_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_custom_credit_types_merchant_id ON openrails.custom_credit_types USING btree (merchant_id);


--
-- Name: idx_customers_merchant; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_customers_merchant ON openrails.customers USING btree (merchant_id);


--
-- Name: idx_entitlements_customer_active_window; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_entitlements_customer_active_window ON openrails.entitlements USING btree (merchant_id, customer_id, entitlement, start_at, end_at) WHERE ((customer_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL));


--
-- Name: idx_entitlements_grace_by_subscription_live; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_entitlements_grace_by_subscription_live ON openrails.entitlements USING btree (source_id, entitlement, start_at, end_at) WHERE ((source_type = 'grace'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));


--
-- Name: idx_entitlements_grant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_entitlements_grant_id ON openrails.entitlements USING btree (grant_id) WHERE (grant_id IS NOT NULL);


--
-- Name: idx_entitlements_live_by_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_entitlements_live_by_id ON openrails.entitlements USING btree (id) WHERE ((revoked_at IS NULL) AND (deleted_at IS NULL));


--
-- Name: idx_entitlements_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_entitlements_merchant_id ON openrails.entitlements USING btree (merchant_id);


--
-- Name: idx_entitlements_one_off_source_live; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_entitlements_one_off_source_live ON openrails.entitlements USING btree (source_id, entitlement) WHERE ((source_type = 'one_off'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));


--
-- Name: idx_entitlements_reverse_active; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_entitlements_reverse_active ON openrails.entitlements USING btree (merchant_id, entitlement, customer_id) WHERE ((revoked_at IS NULL) AND (deleted_at IS NULL));


--
-- Name: idx_entitlements_source; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_entitlements_source ON openrails.entitlements USING btree (source_type, source_id) WHERE (source_id IS NOT NULL);


--
-- Name: idx_entitlements_subscription_source_live; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_entitlements_subscription_source_live ON openrails.entitlements USING btree (source_id, entitlement, end_at) WHERE ((source_type = 'subscription'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));


--
-- Name: idx_external_provider_mutation_logs_merchant_created; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_external_provider_mutation_logs_merchant_created ON openrails.external_provider_mutation_logs USING btree (merchant_id, created_at DESC);


--
-- Name: idx_external_provider_mutation_logs_provider_account; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_external_provider_mutation_logs_provider_account ON openrails.external_provider_mutation_logs USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: idx_external_provider_mutation_logs_provider_intent; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_external_provider_mutation_logs_provider_intent ON openrails.external_provider_mutation_logs USING btree (provider_intent_id) WHERE (provider_intent_id IS NOT NULL);


--
-- Name: idx_external_provider_mutation_logs_provider_phase; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_external_provider_mutation_logs_provider_phase ON openrails.external_provider_mutation_logs USING btree (provider, phase, created_at DESC);


--
-- Name: idx_grants_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_grants_customer ON openrails.grants USING btree (merchant_id, customer_id);


--
-- Name: idx_grants_customer_kind; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_grants_customer_kind ON openrails.grants USING btree (merchant_id, customer_id, kind) WHERE (event = 'grant'::text);


--
-- Name: idx_grants_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_grants_merchant_id ON openrails.grants USING btree (merchant_id);


--
-- Name: idx_grants_source; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_grants_source ON openrails.grants USING btree (merchant_id, source_type, source_id) WHERE (source_id <> ''::text);


--
-- Name: idx_grants_supersedes; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_grants_supersedes ON openrails.grants USING btree (supersedes_id) WHERE (supersedes_id IS NOT NULL);


--
-- Name: idx_invoice_payments_provider_account; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_invoice_payments_provider_account ON openrails.invoice_payments USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: idx_invoices_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_invoices_customer ON openrails.invoices USING btree (customer_id, period_from DESC);


--
-- Name: idx_ledger_accounts_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_ledger_accounts_customer ON openrails.ledger_accounts USING btree (customer_id) WHERE (customer_id IS NOT NULL);


--
-- Name: idx_ledger_accounts_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_ledger_accounts_merchant_id ON openrails.ledger_accounts USING btree (merchant_id);


--
-- Name: idx_ledger_transfers_credit; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_ledger_transfers_credit ON openrails.ledger_transfers USING btree (credit_account_id);


--
-- Name: idx_ledger_transfers_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_ledger_transfers_customer ON openrails.ledger_transfers USING btree (merchant_id, customer_id, currency, created_at DESC) WHERE (customer_id IS NOT NULL);


--
-- Name: idx_ledger_transfers_debit; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_ledger_transfers_debit ON openrails.ledger_transfers USING btree (debit_account_id);


--
-- Name: idx_ledger_transfers_grant; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_ledger_transfers_grant ON openrails.ledger_transfers USING btree (merchant_id, grant_id) WHERE (grant_id IS NOT NULL);


--
-- Name: idx_ledger_transfers_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_ledger_transfers_merchant_id ON openrails.ledger_transfers USING btree (merchant_id);


--
-- Name: idx_ledger_transfers_source; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_ledger_transfers_source ON openrails.ledger_transfers USING btree (merchant_id, source, source_id) WHERE (source IS NOT NULL);


--
-- Name: idx_merchant_credential_audit_merchant; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_merchant_credential_audit_merchant ON openrails.merchant_credential_audit USING btree (merchant_id, created_at DESC);


--
-- Name: idx_merchant_exports_merchant; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_merchant_exports_merchant ON openrails.merchant_exports USING btree (merchant_id, created_at DESC);


--
-- Name: idx_merchants_permission_group_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_merchants_permission_group_id ON openrails.merchants USING btree (permission_group_id) WHERE (permission_group_id IS NOT NULL);


--
-- Name: idx_notification_queue_created_at; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_notification_queue_created_at ON openrails.notification_queue USING btree (created_at);


--
-- Name: idx_notification_queue_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_notification_queue_customer ON openrails.notification_queue USING btree (customer_id) WHERE (customer_id IS NOT NULL);


--
-- Name: idx_notification_queue_event_type; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_notification_queue_event_type ON openrails.notification_queue USING btree (event_type);


--
-- Name: idx_notification_queue_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_notification_queue_merchant_id ON openrails.notification_queue USING btree (merchant_id);


--
-- Name: idx_notification_queue_seen; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_notification_queue_seen ON openrails.notification_queue USING btree (seen);


--
-- Name: idx_payment_methods_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payment_methods_customer ON openrails.payment_methods USING btree (customer_id) WHERE (customer_id IS NOT NULL);


--
-- Name: idx_payment_methods_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payment_methods_merchant_id ON openrails.payment_methods USING btree (merchant_id);


--
-- Name: idx_payment_methods_rail; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payment_methods_rail ON openrails.payment_methods USING btree (rail);


--
-- Name: idx_payment_methods_provider_account; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payment_methods_provider_account ON openrails.payment_methods USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: idx_payment_methods_vault_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payment_methods_vault_id ON openrails.payment_methods USING btree (vault_id);


--
-- Name: idx_payments_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payments_customer ON openrails.payments USING btree (customer_id) WHERE (customer_id IS NOT NULL);


--
-- Name: idx_payments_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payments_merchant_id ON openrails.payments USING btree (merchant_id);


--
-- Name: idx_payments_price_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payments_price_id ON openrails.payments USING btree (price_id);


--
-- Name: idx_payments_rail; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payments_rail ON openrails.payments USING btree (rail);


--
-- Name: idx_payments_provider_account; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payments_provider_account ON openrails.payments USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: idx_payments_purchased_at; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payments_purchased_at ON openrails.payments USING btree (purchased_at);


--
-- Name: idx_payments_refunded_payment_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payments_refunded_payment_id ON openrails.payments USING btree (refunded_payment_id);


--
-- Name: idx_payments_subscription_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_payments_subscription_id ON openrails.payments USING btree (subscription_id);


--
-- Name: idx_prices_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_prices_merchant_id ON openrails.prices USING btree (merchant_id);


--
-- Name: idx_prices_rails; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_prices_rails ON openrails.prices USING gin (rails);


--
-- Name: idx_prices_product_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_prices_product_id ON openrails.prices USING btree (product_id);


--
-- Name: idx_prices_status; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_prices_status ON openrails.prices USING btree (status);


--
-- Name: idx_rail_customers_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_rail_customers_customer ON openrails.rail_customers USING btree (customer_id) WHERE (customer_id IS NOT NULL);


--
-- Name: idx_rail_customers_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_rail_customers_merchant_id ON openrails.rail_customers USING btree (merchant_id);


--
-- Name: idx_rail_customers_provider_account; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_rail_customers_provider_account ON openrails.rail_customers USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: idx_product_entitlement_features_feature; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_product_entitlement_features_feature ON openrails.product_entitlement_features USING btree (merchant_id, entitlement_feature_id);


--
-- Name: idx_product_entitlement_features_product; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_product_entitlement_features_product ON openrails.product_entitlement_features USING btree (merchant_id, product_id);


--
-- Name: idx_products_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_products_merchant_id ON openrails.products USING btree (merchant_id);


--
-- Name: idx_products_slug; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_products_slug ON openrails.products USING btree (slug);


--
-- Name: idx_products_status; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_products_status ON openrails.products USING btree (status);


--
-- Name: idx_products_tier_group; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_products_tier_group ON openrails.products USING btree (tier_group) WHERE (tier_group IS NOT NULL);


--
-- Name: idx_provider_accounts_merchant; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_provider_accounts_merchant ON openrails.provider_accounts USING btree (merchant_id);


--
-- Name: idx_provider_intents_due; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_provider_intents_due ON openrails.provider_intents USING btree (next_attempt_at) WHERE (status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'failed_retryable'::text, 'unknown_needs_verify'::text]));


--
-- Name: idx_provider_intents_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_provider_intents_merchant_id ON openrails.provider_intents USING btree (merchant_id);


--
-- Name: idx_provider_intents_provider_account; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_provider_intents_provider_account ON openrails.provider_intents USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: idx_provider_intents_subscription; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_provider_intents_subscription ON openrails.provider_intents USING btree (subscription_id) WHERE (subscription_id IS NOT NULL);


--
-- Name: idx_provider_refresh_watermarks_account; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_provider_refresh_watermarks_account ON openrails.provider_refresh_watermarks USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: idx_provider_refresh_watermarks_provider; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_provider_refresh_watermarks_provider ON openrails.provider_refresh_watermarks USING btree (provider, event_domain, watermark_at);


--
-- Name: idx_reconciliation_findings_actionable; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_reconciliation_findings_actionable ON openrails.reconciliation_findings USING btree (finding_type) WHERE (status = ANY (ARRAY['reconcile_required'::text, 'requires_review'::text]));


--
-- Name: idx_reconciliation_findings_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_reconciliation_findings_merchant_id ON openrails.reconciliation_findings USING btree (merchant_id);


--
-- Name: idx_reconciliation_findings_requires_review; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_reconciliation_findings_requires_review ON openrails.reconciliation_findings USING btree (last_seen_at DESC) WHERE (status = 'requires_review'::text);


--
-- Name: idx_reconciliation_runs_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_reconciliation_runs_merchant_id ON openrails.reconciliation_runs USING btree (merchant_id);


--
-- Name: idx_reconciliation_runs_started_at; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_reconciliation_runs_started_at ON openrails.reconciliation_runs USING btree (started_at DESC);


--
-- Name: idx_solana_subscriptions_due; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_solana_subscriptions_due ON openrails.solana_subscriptions USING btree (merchant_id, next_pull_at) WHERE (status = 'active'::text);


--
-- Name: idx_solana_subscriptions_subscription_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_solana_subscriptions_subscription_id ON openrails.solana_subscriptions USING btree (subscription_id);


--
-- Name: idx_subscriptions_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_customer ON openrails.subscriptions USING btree (customer_id) WHERE (customer_id IS NOT NULL);


--
-- Name: idx_subscriptions_due_dunning; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_due_dunning ON openrails.subscriptions USING btree (next_retry_at, rail) WHERE ((status = 'past_due'::openrails.subscription_status) AND (next_retry_at IS NOT NULL));


--
-- Name: idx_subscriptions_grace_ends_at; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_grace_ends_at ON openrails.subscriptions USING btree (grace_ends_at) WHERE (grace_ends_at IS NOT NULL);


--
-- Name: idx_subscriptions_merchant_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_merchant_id ON openrails.subscriptions USING btree (merchant_id);


--
-- Name: idx_subscriptions_next_retry_at; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_next_retry_at ON openrails.subscriptions USING btree (next_retry_at) WHERE (next_retry_at IS NOT NULL);


--
-- Name: idx_subscriptions_payment_method_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_payment_method_id ON openrails.subscriptions USING btree (payment_method_id);


--
-- Name: idx_subscriptions_price_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_price_id ON openrails.subscriptions USING btree (price_id);


--
-- Name: idx_subscriptions_rail; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_rail ON openrails.subscriptions USING btree (rail);


--
-- Name: idx_subscriptions_rail_subscription; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_rail_subscription ON openrails.subscriptions USING btree (rail, rail_subscription_id);


--
-- Name: idx_subscriptions_product_id; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_product_id ON openrails.subscriptions USING btree (product_id);


--
-- Name: idx_subscriptions_provider_account; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_provider_account ON openrails.subscriptions USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: idx_subscriptions_status; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_subscriptions_status ON openrails.subscriptions USING btree (status);


--
-- Name: idx_usage_events_customer_time; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_usage_events_customer_time ON openrails.usage_events USING btree (customer_id, occurred_at);


--
-- Name: idx_usage_events_invoker; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_usage_events_invoker ON openrails.usage_events USING btree (merchant_id, invoker_id, occurred_at DESC);


--
-- Name: idx_usdc_funding_sessions_checkout; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_usdc_funding_sessions_checkout ON openrails.usdc_funding_sessions USING btree (merchant_id, checkout_session_id) WHERE (checkout_session_id IS NOT NULL);


--
-- Name: idx_usdc_funding_sessions_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_usdc_funding_sessions_customer ON openrails.usdc_funding_sessions USING btree (merchant_id, customer_id, created_at DESC);


--
-- Name: idx_usdc_funding_sessions_idempotency; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX idx_usdc_funding_sessions_idempotency ON openrails.usdc_funding_sessions USING btree (merchant_id, customer_id, idempotency_key) WHERE ((idempotency_key IS NOT NULL) AND (btrim(idempotency_key) <> ''::text));


--
-- Name: idx_usdc_funding_sessions_provider_session; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX idx_usdc_funding_sessions_provider_session ON openrails.usdc_funding_sessions USING btree (provider, provider_session_id) WHERE (provider_session_id IS NOT NULL);


--
-- Name: ix_invoice_items_invoice; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX ix_invoice_items_invoice ON openrails.invoice_items USING btree (merchant_id, invoice_id);


--
-- Name: ix_invoice_items_pending; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX ix_invoice_items_pending ON openrails.invoice_items USING btree (merchant_id, customer_id, currency, invoice_at) WHERE ((invoice_id IS NULL) AND (status = 'pending'::text));


--
-- Name: ix_invoice_payments_invoice; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX ix_invoice_payments_invoice ON openrails.invoice_payments USING btree (merchant_id, invoice_id, created_at DESC);


--
-- Name: ix_invoices_open_due; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX ix_invoices_open_due ON openrails.invoices USING btree (merchant_id, customer_id, currency, due_at) WHERE ((status = ANY (ARRAY['open'::text, 'past_due'::text])) AND (amount_due > 0));


--
-- Name: ix_invoices_payer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX ix_invoices_payer ON openrails.invoices USING btree (merchant_id, customer_id, period_from DESC);


--
-- Name: ix_usage_events_payer_time; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX ix_usage_events_payer_time ON openrails.usage_events USING btree (merchant_id, customer_id, occurred_at);


--
-- Name: ix_usage_events_payer_type_time; Type: INDEX; Schema: openrails; Owner: -
--

CREATE INDEX ix_usage_events_payer_type_time ON openrails.usage_events USING btree (merchant_id, customer_id, event_type, occurred_at);


--
-- Name: uq_catalog_drift_open; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_catalog_drift_open ON openrails.catalog_drift_events USING btree (provider, kind, openrails_resource_type, COALESCE(openrails_resource_id, ''::text), COALESCE(external_resource_id, ''::text), COALESCE(field, ''::text)) WHERE (resolved_at IS NULL);


--
-- Name: uq_customers_merchant_subject; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_customers_merchant_subject ON openrails.customers USING btree (merchant_id, subject) WHERE (subject IS NOT NULL);


--
-- Name: uq_entitlements_customer_active; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_entitlements_customer_active ON openrails.entitlements USING btree (merchant_id, customer_id, entitlement) WHERE ((customer_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL) AND (end_at IS NULL));


--
-- Name: uq_grants_termination; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_grants_termination ON openrails.grants USING btree (supersedes_id) WHERE ((supersedes_id IS NOT NULL) AND (event = ANY (ARRAY['revoke'::text, 'expire'::text, 'supersede'::text])));


--
-- Name: uq_invoice_items_source; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_invoice_items_source ON openrails.invoice_items USING btree (merchant_id, customer_id, currency, source_type, source_id);


--
-- Name: uq_invoice_payments_money_transaction; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_invoice_payments_money_transaction ON openrails.invoice_payments USING btree (merchant_id, money_transaction_id) WHERE (money_transaction_id IS NOT NULL);


--
-- Name: uq_invoices_period; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_invoices_period ON openrails.invoices USING btree (merchant_id, customer_id, currency, period_from, period_to);


--
-- Name: uq_ledger_accounts_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_ledger_accounts_customer ON openrails.ledger_accounts USING btree (merchant_id, customer_id, account_type, currency) WHERE (customer_id IS NOT NULL);


--
-- Name: uq_ledger_accounts_system; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_ledger_accounts_system ON openrails.ledger_accounts USING btree (merchant_id, account_type, currency) WHERE (customer_id IS NULL);


--
-- Name: uq_money_settings_payer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_money_settings_payer ON openrails.money_settings USING btree (merchant_id, customer_id, currency);


--
-- Name: uq_payer_spend_limits_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_payer_spend_limits_customer ON openrails.payer_spend_limits USING btree (merchant_id, customer_id, tier) WHERE (customer_id IS NOT NULL);


--
-- Name: uq_payer_spend_limits_merchant_default; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_payer_spend_limits_merchant_default ON openrails.payer_spend_limits USING btree (merchant_id, tier) WHERE (customer_id IS NULL);


--
-- Name: uq_payment_blocklist; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_payment_blocklist ON openrails.payment_blocklist USING btree (merchant_id, kind, value);


--
-- Name: uq_payment_methods_customer_vault_legacy; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_payment_methods_customer_vault_legacy ON openrails.payment_methods USING btree (merchant_id, customer_id, vault_id) WHERE (provider_account_id IS NULL);


--
-- Name: uq_payment_methods_merchant_rail_vault_legacy; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_payment_methods_merchant_rail_vault_legacy ON openrails.payment_methods USING btree (merchant_id, rail, vault_id) WHERE (provider_account_id IS NULL);


--
-- Name: uq_payment_methods_provider_account_vault; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_payment_methods_provider_account_vault ON openrails.payment_methods USING btree (merchant_id, provider_account_id, vault_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: uq_payments_merchant_rail_transaction_legacy; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_payments_merchant_rail_transaction_legacy ON openrails.payments USING btree (merchant_id, rail, transaction_id) WHERE (provider_account_id IS NULL);


--
-- Name: uq_payments_provider_account_transaction; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_payments_provider_account_transaction ON openrails.payments USING btree (merchant_id, provider_account_id, transaction_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: uq_prices_id_product_merchant; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_prices_id_product_merchant ON openrails.prices USING btree (id, product_id, merchant_id);


--
-- Name: uq_rail_customers_customer_rail_legacy; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_rail_customers_customer_rail_legacy ON openrails.rail_customers USING btree (merchant_id, customer_id, rail) WHERE (provider_account_id IS NULL);


--
-- Name: uq_rail_customers_customer_provider_account; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_rail_customers_customer_provider_account ON openrails.rail_customers USING btree (merchant_id, customer_id, provider_account_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: uq_rail_customers_merchant_rail_customer_legacy; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_rail_customers_merchant_rail_customer_legacy ON openrails.rail_customers USING btree (merchant_id, rail, rail_customer_id) WHERE (provider_account_id IS NULL);


--
-- Name: uq_rail_customers_provider_account_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_rail_customers_provider_account_customer ON openrails.rail_customers USING btree (merchant_id, provider_account_id, rail_customer_id) WHERE (provider_account_id IS NOT NULL);


--
-- Name: uq_provider_accounts_enabled_primary; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_provider_accounts_enabled_primary ON openrails.provider_accounts USING btree (merchant_id, provider_type, environment) WHERE ((role = 'primary'::text) AND (status = 'enabled'::text));


--
-- Name: uq_provider_accounts_identity; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_provider_accounts_identity ON openrails.provider_accounts USING btree (merchant_id, provider_type, environment, account_id);


--
-- Name: uq_provider_intents_merchant_idempotency_key; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_provider_intents_merchant_idempotency_key ON openrails.provider_intents USING btree (merchant_id, idempotency_key);


--
-- Name: uq_reconciliation_findings_identity; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_reconciliation_findings_identity ON openrails.reconciliation_findings USING btree (merchant_id, finding_type, subject_key);


--
-- Name: uq_reconciliation_state_identity; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_reconciliation_state_identity ON openrails.reconciliation_state USING btree (merchant_id, source_domain);


--
-- Name: uq_subscriptions_customer_product_lifecycle; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_subscriptions_customer_product_lifecycle ON openrails.subscriptions USING btree (merchant_id, customer_id, product_id) WHERE (status = ANY (ARRAY['active'::openrails.subscription_status, 'pending'::openrails.subscription_status, 'past_due'::openrails.subscription_status]));


--
-- Name: uq_subscriptions_customer_tier_group_active; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_subscriptions_customer_tier_group_active ON openrails.subscriptions USING btree (customer_id, tier_group) WHERE ((status = ANY (ARRAY['active'::openrails.subscription_status, 'pending'::openrails.subscription_status])) AND (tier_group IS NOT NULL));


--
-- Name: uq_subscriptions_merchant_rail_subscription_id_legacy; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_subscriptions_merchant_rail_subscription_id_legacy ON openrails.subscriptions USING btree (merchant_id, rail, rail_subscription_id) WHERE ((provider_account_id IS NULL) AND (rail_subscription_id <> ''::text));


--
-- Name: uq_subscriptions_provider_account_subscription; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_subscriptions_provider_account_subscription ON openrails.subscriptions USING btree (merchant_id, provider_account_id, rail_subscription_id) WHERE ((provider_account_id IS NOT NULL) AND (rail_subscription_id <> ''::text));


--
-- Name: uq_tier_schedules_customer; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_tier_schedules_customer ON openrails.tier_schedules USING btree (merchant_id, customer_id, owner, currency) WHERE (customer_id IS NOT NULL);


--
-- Name: uq_tier_schedules_merchant_default; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_tier_schedules_merchant_default ON openrails.tier_schedules USING btree (merchant_id, owner, currency) WHERE (customer_id IS NULL);


--
-- Name: uq_usage_events_idem; Type: INDEX; Schema: openrails; Owner: -
--

CREATE UNIQUE INDEX uq_usage_events_idem ON openrails.usage_events USING btree (merchant_id, customer_id, currency, event_type, source, source_id);


--
-- Name: ledger_transfers trg_ledger_transfers_apply_counters; Type: TRIGGER; Schema: openrails; Owner: -
--

CREATE TRIGGER trg_ledger_transfers_apply_counters BEFORE INSERT ON openrails.ledger_transfers FOR EACH ROW EXECUTE FUNCTION openrails.ledger_transfers_apply_counters();


--
-- Name: subscriptions trg_subscriptions_set_tier_group; Type: TRIGGER; Schema: openrails; Owner: -
--

CREATE TRIGGER trg_subscriptions_set_tier_group BEFORE INSERT OR UPDATE OF product_id ON openrails.subscriptions FOR EACH ROW EXECUTE FUNCTION openrails.subscriptions_set_tier_group();


--
-- Name: checkout_sessions checkout_sessions_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: checkout_sessions checkout_sessions_merchant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;


--
-- Name: checkout_sessions checkout_sessions_payment_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_payment_id_fkey FOREIGN KEY (payment_id) REFERENCES openrails.payments(id);


--
-- Name: checkout_sessions checkout_sessions_price_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_price_id_fkey FOREIGN KEY (price_id) REFERENCES openrails.prices(id);


--
-- Name: checkout_sessions checkout_sessions_provider_account_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);


--
-- Name: checkout_sessions checkout_sessions_subscription_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id);


--
-- Name: customers customers_merchant_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.customers
    ADD CONSTRAINT customers_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id);


--
-- Name: entitlement_features entitlement_features_merchant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.entitlement_features
    ADD CONSTRAINT entitlement_features_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;


--
-- Name: entitlements entitlements_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.entitlements
    ADD CONSTRAINT entitlements_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: entitlements entitlements_grant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.entitlements
    ADD CONSTRAINT entitlements_grant_fk FOREIGN KEY (grant_id) REFERENCES openrails.grants(id);


--
-- Name: external_provider_mutation_logs external_provider_mutation_logs_merchant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.external_provider_mutation_logs
    ADD CONSTRAINT external_provider_mutation_logs_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;


--
-- Name: external_provider_mutation_logs external_provider_mutation_logs_provider_account_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.external_provider_mutation_logs
    ADD CONSTRAINT external_provider_mutation_logs_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id) ON DELETE SET NULL;


--
-- Name: external_provider_mutation_logs external_provider_mutation_logs_provider_intent_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.external_provider_mutation_logs
    ADD CONSTRAINT external_provider_mutation_logs_provider_intent_fk FOREIGN KEY (provider_intent_id) REFERENCES openrails.provider_intents(id) ON DELETE SET NULL;


--
-- Name: grants grants_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: grants grants_merchant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;


--
-- Name: grants grants_payment_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_payment_fk FOREIGN KEY (payment_id) REFERENCES openrails.payments(id);


--
-- Name: grants grants_product_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id);


--
-- Name: grants grants_supersedes_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_supersedes_fk FOREIGN KEY (supersedes_id) REFERENCES openrails.grants(id);


--
-- Name: invoice_items invoice_items_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoice_items
    ADD CONSTRAINT invoice_items_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: invoice_items invoice_items_invoice_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoice_items
    ADD CONSTRAINT invoice_items_invoice_fk FOREIGN KEY (invoice_id) REFERENCES openrails.invoices(id) ON DELETE SET NULL;


--
-- Name: invoice_payments invoice_payments_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoice_payments
    ADD CONSTRAINT invoice_payments_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: invoice_payments invoice_payments_invoice_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoice_payments
    ADD CONSTRAINT invoice_payments_invoice_fk FOREIGN KEY (invoice_id) REFERENCES openrails.invoices(id) ON DELETE CASCADE;


--
-- Name: invoice_payments invoice_payments_provider_account_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoice_payments
    ADD CONSTRAINT invoice_payments_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);


--
-- Name: invoices invoices_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoices
    ADD CONSTRAINT invoices_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: invoker_spend_limits invoker_spend_limits_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.invoker_spend_limits
    ADD CONSTRAINT invoker_spend_limits_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: ledger_accounts ledger_accounts_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.ledger_accounts
    ADD CONSTRAINT ledger_accounts_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: ledger_transfers ledger_transfers_credit_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.ledger_transfers
    ADD CONSTRAINT ledger_transfers_credit_fk FOREIGN KEY (credit_account_id) REFERENCES openrails.ledger_accounts(id);


--
-- Name: ledger_transfers ledger_transfers_debit_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.ledger_transfers
    ADD CONSTRAINT ledger_transfers_debit_fk FOREIGN KEY (debit_account_id) REFERENCES openrails.ledger_accounts(id);


--
-- Name: merchant_configurations merchant_configurations_merchant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.merchant_configurations
    ADD CONSTRAINT merchant_configurations_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id);


--
-- Name: money_settings money_settings_auto_topup_payment_method_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.money_settings
    ADD CONSTRAINT money_settings_auto_topup_payment_method_fk FOREIGN KEY (auto_topup_payment_method_id) REFERENCES openrails.payment_methods(id) ON DELETE SET NULL;


--
-- Name: money_settings money_settings_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.money_settings
    ADD CONSTRAINT money_settings_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: notification_queue notification_queue_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.notification_queue
    ADD CONSTRAINT notification_queue_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: payer_spend_limits payer_spend_limits_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payer_spend_limits
    ADD CONSTRAINT payer_spend_limits_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: payment_blocklist payment_blocklist_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payment_blocklist
    ADD CONSTRAINT payment_blocklist_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: payment_methods payment_methods_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payment_methods
    ADD CONSTRAINT payment_methods_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: payment_methods payment_methods_merchant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payment_methods
    ADD CONSTRAINT payment_methods_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;


--
-- Name: payment_methods payment_methods_provider_account_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payment_methods
    ADD CONSTRAINT payment_methods_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);


--
-- Name: payments payments_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: payments payments_price_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_price_id_fkey FOREIGN KEY (price_id) REFERENCES openrails.prices(id);


--
-- Name: payments payments_provider_account_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);


--
-- Name: payments payments_refunded_payment_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_refunded_payment_id_fkey FOREIGN KEY (refunded_payment_id) REFERENCES openrails.payments(id);


--
-- Name: payments payments_subscription_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE SET NULL;


--
-- Name: prices prices_merchant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.prices
    ADD CONSTRAINT prices_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;


--
-- Name: prices prices_product_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.prices
    ADD CONSTRAINT prices_product_id_fkey FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE RESTRICT;


--
-- Name: rail_customers rail_customers_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.rail_customers
    ADD CONSTRAINT rail_customers_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: rail_customers rail_customers_provider_account_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.rail_customers
    ADD CONSTRAINT rail_customers_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);


--
-- Name: product_entitlement_features product_entitlement_features_entitlement_feature_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.product_entitlement_features
    ADD CONSTRAINT product_entitlement_features_entitlement_feature_id_fkey FOREIGN KEY (entitlement_feature_id) REFERENCES openrails.entitlement_features(id) ON DELETE CASCADE;


--
-- Name: product_entitlement_features product_entitlement_features_merchant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.product_entitlement_features
    ADD CONSTRAINT product_entitlement_features_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;


--
-- Name: product_entitlement_features product_entitlement_features_product_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.product_entitlement_features
    ADD CONSTRAINT product_entitlement_features_product_id_fkey FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;


--
-- Name: products products_merchant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.products
    ADD CONSTRAINT products_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;


--
-- Name: provider_accounts provider_accounts_merchant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.provider_accounts
    ADD CONSTRAINT provider_accounts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;


--
-- Name: provider_intents provider_intents_provider_account_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.provider_intents
    ADD CONSTRAINT provider_intents_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);


--
-- Name: provider_refresh_watermarks provider_refresh_watermarks_merchant_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.provider_refresh_watermarks
    ADD CONSTRAINT provider_refresh_watermarks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;


--
-- Name: provider_refresh_watermarks provider_refresh_watermarks_provider_account_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.provider_refresh_watermarks
    ADD CONSTRAINT provider_refresh_watermarks_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id) ON DELETE SET NULL;


--
-- Name: solana_subscriptions solana_subscriptions_subscription_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE CASCADE;


--
-- Name: subscriptions subscriptions_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: subscriptions subscriptions_payment_method_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_payment_method_id_fkey FOREIGN KEY (payment_method_id) REFERENCES openrails.payment_methods(id) ON DELETE SET NULL;


--
-- Name: subscriptions subscriptions_price_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_price_id_fkey FOREIGN KEY (price_id) REFERENCES openrails.prices(id);


--
-- Name: subscriptions subscriptions_price_product_merchant_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_price_product_merchant_fkey FOREIGN KEY (price_id, product_id, merchant_id) REFERENCES openrails.prices(id, product_id, merchant_id);


--
-- Name: subscriptions subscriptions_product_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_product_id_fkey FOREIGN KEY (product_id) REFERENCES openrails.products(id);


--
-- Name: subscriptions subscriptions_provider_account_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id);


--
-- Name: subscriptions subscriptions_scheduled_price_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_scheduled_price_id_fkey FOREIGN KEY (scheduled_price_id) REFERENCES openrails.prices(id);


--
-- Name: tier_schedules tier_schedules_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.tier_schedules
    ADD CONSTRAINT tier_schedules_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: usage_events usage_events_customer_fk; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.usage_events
    ADD CONSTRAINT usage_events_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);


--
-- Name: usdc_funding_sessions usdc_funding_sessions_checkout_session_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.usdc_funding_sessions
    ADD CONSTRAINT usdc_funding_sessions_checkout_session_id_fkey FOREIGN KEY (checkout_session_id) REFERENCES openrails.checkout_sessions(id) ON DELETE SET NULL;


--
-- Name: usdc_funding_sessions usdc_funding_sessions_customer_id_fkey; Type: FK CONSTRAINT; Schema: openrails; Owner: -
--

ALTER TABLE ONLY openrails.usdc_funding_sessions
    ADD CONSTRAINT usdc_funding_sessions_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE CASCADE;


--
-- Name: catalog_drift_events; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.catalog_drift_events ENABLE ROW LEVEL SECURITY;

--
-- Name: checkout_sessions; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.checkout_sessions ENABLE ROW LEVEL SECURITY;

--
-- Name: custom_credit_types; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.custom_credit_types ENABLE ROW LEVEL SECURITY;

--
-- Name: customers; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.customers ENABLE ROW LEVEL SECURITY;

--
-- Name: entitlement_features; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.entitlement_features ENABLE ROW LEVEL SECURITY;

--
-- Name: entitlements; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.entitlements ENABLE ROW LEVEL SECURITY;

--
-- Name: external_provider_mutation_logs; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.external_provider_mutation_logs ENABLE ROW LEVEL SECURITY;

--
-- Name: grants; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.grants ENABLE ROW LEVEL SECURITY;

--
-- Name: invoice_items; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.invoice_items ENABLE ROW LEVEL SECURITY;

--
-- Name: invoice_payments; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.invoice_payments ENABLE ROW LEVEL SECURITY;

--
-- Name: invoices; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.invoices ENABLE ROW LEVEL SECURITY;

--
-- Name: invoker_spend_limits; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.invoker_spend_limits ENABLE ROW LEVEL SECURITY;

--
-- Name: ledger_accounts; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.ledger_accounts ENABLE ROW LEVEL SECURITY;

--
-- Name: ledger_transfers; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.ledger_transfers ENABLE ROW LEVEL SECURITY;

--
-- Name: merchant_configurations; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.merchant_configurations ENABLE ROW LEVEL SECURITY;

--
-- Name: merchant_credential_audit; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.merchant_credential_audit ENABLE ROW LEVEL SECURITY;

--
-- Name: merchant_deks; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.merchant_deks ENABLE ROW LEVEL SECURITY;

--
-- Name: merchant_exports; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.merchant_exports ENABLE ROW LEVEL SECURITY;

--
-- Name: catalog_drift_events merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.catalog_drift_events USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: checkout_sessions merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.checkout_sessions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: custom_credit_types merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.custom_credit_types USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: customers merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.customers USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: entitlement_features merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.entitlement_features USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: entitlements merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.entitlements USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: external_provider_mutation_logs merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.external_provider_mutation_logs USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: grants merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.grants USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: invoice_items merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.invoice_items USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: invoice_payments merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.invoice_payments USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: invoices merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.invoices USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: invoker_spend_limits merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.invoker_spend_limits USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: ledger_accounts merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.ledger_accounts USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: ledger_transfers merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.ledger_transfers USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: merchant_configurations merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.merchant_configurations USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: merchant_credential_audit merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.merchant_credential_audit USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: merchant_deks merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.merchant_deks USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: merchant_exports merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.merchant_exports USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: merchant_secrets merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.merchant_secrets USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: money_settings merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.money_settings USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: notification_queue merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.notification_queue USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: payer_spend_limits merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.payer_spend_limits USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: payment_blocklist merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.payment_blocklist USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: payment_methods merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.payment_methods USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: payments merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.payments USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: prices merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.prices USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: rail_customers merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.rail_customers USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: product_entitlement_features merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.product_entitlement_features USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: products merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.products USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: provider_accounts merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.provider_accounts USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: provider_intents merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.provider_intents USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: provider_refresh_watermarks merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.provider_refresh_watermarks USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: reconciliation_findings merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.reconciliation_findings USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: reconciliation_runs merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.reconciliation_runs USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: reconciliation_state merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.reconciliation_state USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: solana_subscriptions merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.solana_subscriptions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: subscriptions merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.subscriptions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: tier_schedules merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.tier_schedules USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: usage_events merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.usage_events USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: usdc_funding_sessions merchant_isolation; Type: POLICY; Schema: openrails; Owner: -
--

CREATE POLICY merchant_isolation ON openrails.usdc_funding_sessions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));


--
-- Name: merchant_secrets; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.merchant_secrets ENABLE ROW LEVEL SECURITY;

--
-- Name: money_settings; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.money_settings ENABLE ROW LEVEL SECURITY;

--
-- Name: notification_queue; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.notification_queue ENABLE ROW LEVEL SECURITY;

--
-- Name: payer_spend_limits; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.payer_spend_limits ENABLE ROW LEVEL SECURITY;

--
-- Name: payment_blocklist; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.payment_blocklist ENABLE ROW LEVEL SECURITY;

--
-- Name: payment_methods; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.payment_methods ENABLE ROW LEVEL SECURITY;

--
-- Name: payments; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.payments ENABLE ROW LEVEL SECURITY;

--
-- Name: prices; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.prices ENABLE ROW LEVEL SECURITY;

--
-- Name: rail_customers; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.rail_customers ENABLE ROW LEVEL SECURITY;

--
-- Name: product_entitlement_features; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.product_entitlement_features ENABLE ROW LEVEL SECURITY;

--
-- Name: products; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.products ENABLE ROW LEVEL SECURITY;

--
-- Name: provider_accounts; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.provider_accounts ENABLE ROW LEVEL SECURITY;

--
-- Name: provider_intents; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.provider_intents ENABLE ROW LEVEL SECURITY;

--
-- Name: provider_refresh_watermarks; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.provider_refresh_watermarks ENABLE ROW LEVEL SECURITY;

--
-- Name: reconciliation_findings; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.reconciliation_findings ENABLE ROW LEVEL SECURITY;

--
-- Name: reconciliation_runs; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.reconciliation_runs ENABLE ROW LEVEL SECURITY;

--
-- Name: reconciliation_state; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.reconciliation_state ENABLE ROW LEVEL SECURITY;

--
-- Name: solana_subscriptions; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.solana_subscriptions ENABLE ROW LEVEL SECURITY;

--
-- Name: subscriptions; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.subscriptions ENABLE ROW LEVEL SECURITY;

--
-- Name: tier_schedules; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.tier_schedules ENABLE ROW LEVEL SECURITY;

--
-- Name: usage_events; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.usage_events ENABLE ROW LEVEL SECURITY;

--
-- Name: usdc_funding_sessions; Type: ROW SECURITY; Schema: openrails; Owner: -
--

ALTER TABLE openrails.usdc_funding_sessions ENABLE ROW LEVEL SECURITY;

--
-- Name: SCHEMA openrails; Type: ACL; Schema: -; Owner: -
--

GRANT USAGE ON SCHEMA openrails TO openrails_app;


--
-- Name: TABLE bootstrap_state; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.bootstrap_state TO openrails_app;


--
-- Name: TABLE catalog_drift_events; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_drift_events TO openrails_app;


--
-- Name: TABLE checkout_sessions; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.checkout_sessions TO openrails_app;


--
-- Name: TABLE custom_credit_types; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.custom_credit_types TO openrails_app;


--
-- Name: TABLE customers; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.customers TO openrails_app;


--
-- Name: TABLE entitlement_features; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.entitlement_features TO openrails_app;


--
-- Name: TABLE entitlements; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.entitlements TO openrails_app;


--
-- Name: TABLE external_provider_mutation_logs; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.external_provider_mutation_logs TO openrails_app;


--
-- Name: TABLE grants; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT ON TABLE openrails.grants TO openrails_app;


--
-- Name: TABLE invoice_items; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoice_items TO openrails_app;


--
-- Name: TABLE invoice_payments; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoice_payments TO openrails_app;


--
-- Name: TABLE invoices; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoices TO openrails_app;


--
-- Name: TABLE invoker_spend_limits; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoker_spend_limits TO openrails_app;


--
-- Name: TABLE ledger_accounts; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT ON TABLE openrails.ledger_accounts TO openrails_app;


--
-- Name: TABLE ledger_transfers; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT ON TABLE openrails.ledger_transfers TO openrails_app;


--
-- Name: TABLE merchant_configurations; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_configurations TO openrails_app;


--
-- Name: TABLE merchant_credential_audit; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_credential_audit TO openrails_app;


--
-- Name: TABLE merchant_deks; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_deks TO openrails_app;


--
-- Name: TABLE merchant_exports; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_exports TO openrails_app;


--
-- Name: TABLE merchant_secrets; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_secrets TO openrails_app;


--
-- Name: TABLE merchants; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchants TO openrails_app;


--
-- Name: TABLE money_settings; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.money_settings TO openrails_app;


--
-- Name: TABLE notification_queue; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.notification_queue TO openrails_app;


--
-- Name: TABLE payer_spend_limits; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payer_spend_limits TO openrails_app;


--
-- Name: TABLE payment_blocklist; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payment_blocklist TO openrails_app;


--
-- Name: TABLE payment_methods; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payment_methods TO openrails_app;


--
-- Name: TABLE payments; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payments TO openrails_app;


--
-- Name: TABLE prices; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.prices TO openrails_app;


--
-- Name: TABLE probe_verdicts; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.probe_verdicts TO openrails_app;


--
-- Name: TABLE rail_customers; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.rail_customers TO openrails_app;


--
-- Name: TABLE product_entitlement_features; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.product_entitlement_features TO openrails_app;


--
-- Name: TABLE products; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.products TO openrails_app;


--
-- Name: TABLE provider_accounts; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.provider_accounts TO openrails_app;


--
-- Name: TABLE provider_intents; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.provider_intents TO openrails_app;


--
-- Name: TABLE provider_refresh_watermarks; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.provider_refresh_watermarks TO openrails_app;


--
-- Name: TABLE reconciliation_findings; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.reconciliation_findings TO openrails_app;


--
-- Name: TABLE reconciliation_runs; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.reconciliation_runs TO openrails_app;


--
-- Name: TABLE reconciliation_state; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.reconciliation_state TO openrails_app;


--
-- Name: TABLE solana_subscriptions; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.solana_subscriptions TO openrails_app;


--
-- Name: TABLE subscriptions; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.subscriptions TO openrails_app;


--
-- Name: TABLE tier_schedules; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.tier_schedules TO openrails_app;


--
-- Name: TABLE usage_events; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.usage_events TO openrails_app;


--
-- Name: TABLE usdc_funding_sessions; Type: ACL; Schema: openrails; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.usdc_funding_sessions TO openrails_app;


--
-- Name: DEFAULT PRIVILEGES FOR SEQUENCES; Type: DEFAULT ACL; Schema: openrails; Owner: -
--

ALTER DEFAULT PRIVILEGES FOR ROLE admin IN SCHEMA openrails GRANT SELECT,USAGE ON SEQUENCES TO openrails_app;


--
-- Name: DEFAULT PRIVILEGES FOR TABLES; Type: DEFAULT ACL; Schema: openrails; Owner: -
--

ALTER DEFAULT PRIVILEGES FOR ROLE admin IN SCHEMA openrails GRANT SELECT,INSERT,DELETE,UPDATE ON TABLES TO openrails_app;


--
