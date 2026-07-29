-- =============================================================================
-- OpenRails billing schema — consolidated baseline (squashed through migration
-- 0015, 2026-07-07). Produced from pg_dump --schema-only --no-owner of a fully
-- migrated database, reordered into strict TOPOLOGICAL order (every table's FK
-- targets are created before it — no forward references, no genuine FK cycles
-- exist in this schema) with each table's own constraints, indexes, triggers,
-- RLS policy, and grants grouped immediately beside it.
--
-- migratekit tracks applied migrations by filename, so existing databases skip
-- this file. New migrations start at 0002.
-- =============================================================================

SET statement_timeout = '300s';
SET lock_timeout = '10s';
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

-- Roles are CLUSTER-wide while migratekit's advisory lock is per-DATABASE, so two
-- databases migrating concurrently on one cluster (every parallel integration run,
-- and any two services sharing a cluster) both pass a check-then-act guard and one
-- loses. Let the CREATE race and swallow the duplicate instead.
--
-- BOTH SQLSTATEs are required: a plain sequential duplicate raises duplicate_object
-- (42710), but the loser of a genuine race gets a raw unique_violation (23505) off
-- pg_authid_rolname_index before the friendly check runs.
DO $$
BEGIN
    CREATE ROLE openrails_app NOLOGIN NOBYPASSRLS;
EXCEPTION WHEN duplicate_object OR unique_violation THEN
    NULL;
END $$;

-- btree_gist backs the EXCLUDE constraints on uuid+tstzrange
CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

CREATE SCHEMA IF NOT EXISTS openrails;
GRANT USAGE ON SCHEMA openrails TO openrails_app;

-- ---------------------------------------------------------------------------
-- Cross-schema grants (#764): openrails_app also drives River's job-queue
-- tables (`public`, config.RiverSchema) and AuthKit's `profiles` schema — both
-- sibling-owned, independently-versioned schemas. This runs as step 3 of
-- internal/migrate.RunPostgres, AFTER step 1 (AuthKit migrations, creates
-- `profiles`) and step 2 (River migrations, creates the tables below), so
-- every object granted here already exists on a real boot. A from-scratch or
-- test-only harness that applies OpenRails' own migrations without also
-- running River's/AuthKit's separate migrators must not fail here — the
-- guarded DO block below and the prospective ALTER DEFAULT PRIVILEGES still
-- cover those tables the moment they get created by this same role.
-- ---------------------------------------------------------------------------

-- River (public schema). Least-privilege: name the actual tables the runtime
-- client touches (per riverpgxv5's dbsqlc query set) rather than blanket ALL
-- TABLES IN SCHEMA public. river_migration is deliberately excluded — only
-- River's own migrator (running as the admin/migrate role) ever reads or
-- writes it; the runtime client never touches it.
GRANT USAGE ON SCHEMA public TO openrails_app;
DO $$
BEGIN
  IF to_regclass('public.river_job') IS NOT NULL THEN
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.river_job TO openrails_app;
  END IF;
  IF to_regclass('public.river_job_id_seq') IS NOT NULL THEN
    GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.river_job_id_seq TO openrails_app;
  END IF;
  IF to_regclass('public.river_queue') IS NOT NULL THEN
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.river_queue TO openrails_app;
  END IF;
  IF to_regclass('public.river_leader') IS NOT NULL THEN
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.river_leader TO openrails_app;
  END IF;
END $$;

-- The migratekit ledger table (public.migrations) predates this migration on
-- every deployment (created at the bootstrap step, before AuthKit/River/
-- OpenRails migrations run). The runtime reads it once at boot to confirm
-- migrations are applied (internal/app/build_runtime.go validateDatabase) —
-- read-only, no write path needs it.
GRANT SELECT ON TABLE public.migrations TO openrails_app;

-- A later River version bump's NEW tables must inherit the same grant
-- automatically — both migrators (River's and OpenRails') run as the SAME
-- admin/migrate role that applies this migration, so ALTER DEFAULT PRIVILEGES
-- (scoped to that role, implicitly) covers them without a follow-up migration.
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO openrails_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO openrails_app;

-- AuthKit (profiles schema) — a sibling-owned, independently-versioned schema
-- (github.com/open-rails/authkit's own migrations). Unlike River, granted at
-- the SCHEMA boundary rather than by table name: this repo does not own or
-- want to track AuthKit's internal table shape, exactly as openrails.* is
-- granted at the openrails schema boundary above.
GRANT USAGE ON SCHEMA profiles TO openrails_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA profiles TO openrails_app;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA profiles TO openrails_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA profiles GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO openrails_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA profiles GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO openrails_app;

-- ---------------------------------------------------------------------------
-- Types
-- ---------------------------------------------------------------------------

CREATE TYPE openrails.payment_status AS ENUM (
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
    'past_due',
    'unknown'
);

-- ---------------------------------------------------------------------------
-- Functions
-- ---------------------------------------------------------------------------

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

CREATE FUNCTION openrails.subscriptions_record_status_transition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO openrails.subscription_status_transitions
            (merchant_id, subscription_id, from_status, to_status, cancel_type, occurred_at)
        VALUES (NEW.merchant_id, NEW.id, NULL, NEW.status, NEW.cancel_type, now());
    ELSIF OLD.status IS DISTINCT FROM NEW.status THEN
        INSERT INTO openrails.subscription_status_transitions
            (merchant_id, subscription_id, from_status, to_status, cancel_type, occurred_at)
        VALUES (NEW.merchant_id, NEW.id, OLD.status, NEW.status, NEW.cancel_type, now());
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION openrails.prices_default_key() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    product_key text;
    interval_label text;
BEGIN
    IF NEW.key IS NOT NULL AND btrim(NEW.key) <> '' THEN
        RETURN NEW;
    END IF;
    SELECT key INTO product_key FROM openrails.products WHERE id = NEW.product_id;
    IF product_key IS NULL THEN
        RAISE EXCEPTION 'prices_default_key: product % not found for price %', NEW.product_id, NEW.id;
    END IF;
    IF NOT NEW.auto_renew OR NEW.access_duration_hours IS NULL THEN
        interval_label := 'onetime';
    ELSIF NEW.access_duration_hours = 168 THEN
        interval_label := 'weekly';
    ELSIF NEW.access_duration_hours IN (720, 744) THEN
        interval_label := 'monthly';
    ELSIF NEW.access_duration_hours IN (2160, 2184) THEN
        interval_label := 'quarterly';
    ELSIF NEW.access_duration_hours IN (8760, 8784) THEN
        interval_label := 'yearly';
    ELSE
        interval_label := (NEW.access_duration_hours / 24)::text || 'd';
    END IF;
    NEW.key := product_key || '-' || interval_label;
    RETURN NEW;
END;
$$;


SET default_tablespace = '';

SET default_table_access_method = heap;

-- ---------------------------------------------------------------------------
-- Tables (strict topological order; each with its constraints, indexes,
-- triggers, RLS, and grants)
-- ---------------------------------------------------------------------------

CREATE TABLE openrails.merchants (
    id uuid DEFAULT uuidv7() NOT NULL,
    slug text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    permission_group_id text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    deleted_at timestamp with time zone,
    display_name text,
    api_host text,
    CONSTRAINT merchants_status_check CHECK ((status = ANY (ARRAY['active'::text, 'deleted'::text])))
);

COMMENT ON TABLE openrails.merchants IS 'Merchant / billing-namespace directory: a dumb billing bucket (whose books a row goes on). GLOBAL (control-plane) table, not tenant-scoped. Carries ONLY billing/money-rail state, NO auth. Merchants are registered explicitly; there is no default merchant.';

COMMENT ON COLUMN openrails.merchants.slug IS 'Stable merchant slug used in merchant-scoped routes and resolution.';

COMMENT ON COLUMN openrails.merchants.permission_group_id IS 'The merchant''s own AuthKit permission-group id (#567): a merchant is a top-level `merchant` group, child of `root`. Bare `text`, NO FK into the auth schema (#544 portability guard). NULL in embedded (no control plane). Used to resolve a merchant from its authenticated group id.';

COMMENT ON COLUMN openrails.merchants.display_name IS 'human-readable merchant name for end-user display / invoices; NULL = fall back to slug.';

COMMENT ON COLUMN openrails.merchants.api_host IS 'Canonical Host-header value this merchant resolves from (#734), e.g. "api.acme.example". NULL = no Host resolution for this merchant. Lowercase, no scheme/port.';

ALTER TABLE ONLY openrails.merchants
    ADD CONSTRAINT merchants_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.merchants
    ADD CONSTRAINT uq_merchants_slug UNIQUE (slug);

CREATE INDEX idx_merchants_permission_group_id ON openrails.merchants USING btree (permission_group_id) WHERE (permission_group_id IS NOT NULL);

CREATE UNIQUE INDEX uq_merchants_api_host ON openrails.merchants USING btree (api_host) WHERE ((api_host IS NOT NULL) AND (deleted_at IS NULL));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchants TO openrails_app;

CREATE TABLE openrails.probe_verdicts (
    rail text NOT NULL,
    key_hash text NOT NULL,
    verdict text NOT NULL,
    checked_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT chk_probe_verdicts_verdict CHECK ((verdict = ANY (ARRAY['live'::text, 'simulated'::text])))
);

COMMENT ON TABLE openrails.probe_verdicts IS 'Cached NMI test-mode probe verdicts (#348): one row per (rail, sha256(security_key)). Fresh ''live'' refuses boot from cache, fresh ''simulated'' skips the probe, stale/missing re-probes. RLS-exempt by design: instance-level credential state, not tenant data.';

COMMENT ON COLUMN openrails.probe_verdicts.key_hash IS 'sha256 hex of the rail security key. A rotated key hashes differently, so the cache never answers for a credential it has not seen.';

ALTER TABLE ONLY openrails.probe_verdicts
    ADD CONSTRAINT probe_verdicts_pkey PRIMARY KEY (rail, key_hash);

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.probe_verdicts TO openrails_app;

CREATE TABLE openrails.worker_health (
    worker_kind text NOT NULL,
    registered_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    expected_period_seconds bigint,
    last_success_at timestamp with time zone,
    last_error_at timestamp with time zone,
    last_error text,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    last_alerted_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

COMMENT ON TABLE openrails.worker_health IS '#689 per-River-worker-kind health: last success/error + failure streak, written by the worker middleware. Operator-global control-plane table (no merchant scope, no RLS — see merchants).';

COMMENT ON COLUMN openrails.worker_health.registered_at IS 'First time this kind was seeded (deploy that introduced it) — anchors the never-succeeded-since-deploy alert.';

COMMENT ON COLUMN openrails.worker_health.expected_period_seconds IS 'Declared periodic cadence captured at registration; NULL/0 = on-demand kind (no staleness alerting).';

COMMENT ON COLUMN openrails.worker_health.last_error IS 'Most recent work error, truncated by the writer.';

COMMENT ON COLUMN openrails.worker_health.last_alerted_at IS 'When the health checker last raised a repair alert for this kind (dedup/re-alert pacing).';

ALTER TABLE ONLY openrails.worker_health
    ADD CONSTRAINT worker_health_pkey PRIMARY KEY (worker_kind);

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.worker_health TO openrails_app;

CREATE TABLE openrails.admission_denials_hourly (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    denial_reason text NOT NULL,
    hour_at timestamp with time zone NOT NULL,
    denials bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_adh_denials_positive CHECK ((denials > 0)),
    CONSTRAINT chk_adh_hour_aligned CHECK ((hour_at = date_trunc('hour'::text, hour_at)))
);

ALTER TABLE ONLY openrails.admission_denials_hourly FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.admission_denials_hourly IS '#733 hourly admission-denial aggregates (merchant x payer x reason), flushed periodically from Redis counters — the hot path never writes PG per-request.';

ALTER TABLE ONLY openrails.admission_denials_hourly
    ADD CONSTRAINT admission_denials_hourly_pkey PRIMARY KEY (merchant_id, customer_id, denial_reason, hour_at);

ALTER TABLE ONLY openrails.admission_denials_hourly
    ADD CONSTRAINT adh_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_adh_merchant_hour ON openrails.admission_denials_hourly USING btree (merchant_id, hour_at);

ALTER TABLE openrails.admission_denials_hourly ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.admission_denials_hourly USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,UPDATE ON TABLE openrails.admission_denials_hourly TO openrails_app;

CREATE TABLE openrails.alert_rules (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    template text NOT NULL,
    params jsonb DEFAULT '{}'::jsonb NOT NULL,
    severity text DEFAULT 'warning'::text NOT NULL,
    channels jsonb DEFAULT '[]'::jsonb NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    fired_at timestamp with time zone,
    cleared_at timestamp with time zone,
    last_evaluated_at timestamp with time zone,
    last_value double precision,
    last_detail jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT alert_rules_severity_check CHECK ((severity = ANY (ARRAY['warning'::text, 'critical'::text])))
);

ALTER TABLE ONLY openrails.alert_rules FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.alert_rules IS '#736 per-merchant metric threshold rules. template + params compile to a #733 metrics query the evaluator runs on a slow tick; fired_at/cleared_at are edge-triggered state (fire once on crossing, clear on recrossing).';

COMMENT ON COLUMN openrails.alert_rules.channels IS 'ordered channel refs: [{"type":"in_app"}|{"type":"email"}|{"type":"webhook","webhook_id":"<uuid>"}].';

COMMENT ON COLUMN openrails.alert_rules.fired_at IS 'set when the current active alert opened (NULL = not firing); the evaluator never re-fires while non-NULL.';

ALTER TABLE ONLY openrails.alert_rules
    ADD CONSTRAINT alert_rules_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.alert_rules
    ADD CONSTRAINT alert_rules_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX alert_rules_enabled_merchant_idx ON openrails.alert_rules USING btree (merchant_id) WHERE enabled;

CREATE INDEX alert_rules_merchant_idx ON openrails.alert_rules USING btree (merchant_id);

ALTER TABLE openrails.alert_rules ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.alert_rules USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.alert_rules TO openrails_app;

CREATE TABLE openrails.catalog_credit_balances (
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    unit text NOT NULL,
    expires_hours integer,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT catalog_credit_balances_expires_positive CHECK (((expires_hours IS NULL) OR (expires_hours > 0))),
    CONSTRAINT catalog_credit_balances_key_nonempty CHECK ((btrim(key) <> ''::text)),
    CONSTRAINT catalog_credit_balances_unit_nonempty CHECK ((btrim(unit) <> ''::text))
);

ALTER TABLE ONLY openrails.catalog_credit_balances FORCE ROW LEVEL SECURITY;

ALTER TABLE ONLY openrails.catalog_credit_balances
    ADD CONSTRAINT catalog_credit_balances_pkey PRIMARY KEY (merchant_id, key);

ALTER TABLE ONLY openrails.catalog_credit_balances
    ADD CONSTRAINT catalog_credit_balances_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.catalog_credit_balances ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.catalog_credit_balances USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_credit_balances TO openrails_app;

CREATE TABLE openrails.products (
    id uuid DEFAULT uuidv7() NOT NULL,
    key text NOT NULL,
    display_name text NOT NULL,
    description text,
    entitlements_spec jsonb,
    credits_spec jsonb,
    tier_group character varying(100),
    tier_rank integer DEFAULT 0 NOT NULL,
    archived boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL
);

ALTER TABLE ONLY openrails.products FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.products IS 'Product definitions that can be purchased or subscribed to';

COMMENT ON COLUMN openrails.products.credits_spec IS 'Bundled promo credits spec (amount, expiry, cadence) for subscriptions';

COMMENT ON COLUMN openrails.products.tier_group IS 'Semantic group name for mutually-exclusive products (e.g., "premium"). Products in same group require upgrade/downgrade, not parallel ownership.';

COMMENT ON COLUMN openrails.products.tier_rank IS 'Tier ranking within group. Higher = more premium. Used to determine upgrade (higher rank) vs downgrade (lower rank) direction.';

ALTER TABLE ONLY openrails.products
    ADD CONSTRAINT products_merchant_key_key UNIQUE (merchant_id, key);

ALTER TABLE ONLY openrails.products
    ADD CONSTRAINT products_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.products
    ADD CONSTRAINT products_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_products_archived ON openrails.products USING btree (archived);

CREATE INDEX idx_products_key ON openrails.products USING btree (key);

CREATE INDEX idx_products_merchant_id ON openrails.products USING btree (merchant_id);

CREATE INDEX idx_products_tier_group ON openrails.products USING btree (tier_group) WHERE (tier_group IS NOT NULL);

ALTER TABLE openrails.products ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.products USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.products TO openrails_app;

CREATE TABLE openrails.catalog_credit_purchase_prices (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    ordinal integer NOT NULL,
    credit_key text NOT NULL,
    currency text NOT NULL,
    rails text[] DEFAULT ARRAY[]::text[] NOT NULL,
    input_min bigint DEFAULT 0 NOT NULL,
    input_max bigint DEFAULT 0 NOT NULL,
    round text,
    price jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT catalog_credit_purchase_prices_currency_nonempty CHECK ((btrim(currency) <> ''::text)),
    CONSTRAINT catalog_credit_purchase_prices_input_nonnegative CHECK (((input_min >= 0) AND (input_max >= 0))),
    CONSTRAINT catalog_credit_purchase_prices_input_order CHECK (((input_max = 0) OR (input_min <= input_max))),
    CONSTRAINT catalog_credit_purchase_prices_ordinal_positive CHECK ((ordinal >= 1))
);

ALTER TABLE ONLY openrails.catalog_credit_purchase_prices FORCE ROW LEVEL SECURITY;

ALTER TABLE ONLY openrails.catalog_credit_purchase_prices
    ADD CONSTRAINT catalog_credit_purchase_prices_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.catalog_credit_purchase_prices
    ADD CONSTRAINT catalog_credit_purchase_prices_balance_fk FOREIGN KEY (merchant_id, credit_key) REFERENCES openrails.catalog_credit_balances(merchant_id, key) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.catalog_credit_purchase_prices
    ADD CONSTRAINT catalog_credit_purchase_prices_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.catalog_credit_purchase_prices
    ADD CONSTRAINT catalog_credit_purchase_prices_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;

CREATE UNIQUE INDEX uq_catalog_credit_purchase_prices_product_ordinal ON openrails.catalog_credit_purchase_prices USING btree (merchant_id, product_id, ordinal);

ALTER TABLE openrails.catalog_credit_purchase_prices ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.catalog_credit_purchase_prices USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_credit_purchase_prices TO openrails_app;

CREATE TABLE openrails.catalog_drift_events (
    id uuid DEFAULT uuidv7() NOT NULL,
    rail text NOT NULL,
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
    CONSTRAINT catalog_drift_events_rail_check CHECK ((rail = ANY (ARRAY['stripe'::text, 'nmi'::text])))
);

ALTER TABLE ONLY openrails.catalog_drift_events FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.catalog_drift_events IS 'Alert-only drift/orphan records from the catalog reconciliation loop; resolved via per-price reconcile.';

ALTER TABLE ONLY openrails.catalog_drift_events
    ADD CONSTRAINT catalog_drift_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.catalog_drift_events
    ADD CONSTRAINT catalog_drift_events_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_catalog_drift_events_merchant_id ON openrails.catalog_drift_events USING btree (merchant_id);

CREATE INDEX idx_catalog_drift_events_open ON openrails.catalog_drift_events USING btree (detected_at DESC) WHERE (resolved_at IS NULL);

CREATE UNIQUE INDEX uq_catalog_drift_open ON openrails.catalog_drift_events USING btree (rail, kind, openrails_resource_type, COALESCE(openrails_resource_id, ''::text), COALESCE(external_resource_id, ''::text), COALESCE(field, ''::text)) WHERE (resolved_at IS NULL);

ALTER TABLE openrails.catalog_drift_events ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.catalog_drift_events USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_drift_events TO openrails_app;

CREATE TABLE openrails.catalog_meters (
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    kind text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    event_type text,
    value_property text,
    aggregation text,
    unit text,
    group_by jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT catalog_meters_aggregation_check CHECK (((aggregation IS NULL) OR (aggregation = ANY (ARRAY['sum'::text, 'count'::text, 'max'::text, 'min'::text, 'unique_count'::text, 'latest'::text])))),
    CONSTRAINT catalog_meters_key_nonempty CHECK ((btrim(key) <> ''::text)),
    CONSTRAINT catalog_meters_kind_check CHECK (((kind IS NULL) OR (kind = ANY (ARRAY['counter'::text, 'gauge'::text]))))
);

ALTER TABLE ONLY openrails.catalog_meters FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.catalog_meters IS '#599 billing meter registry. Meters are billed-later usage streams, distinct from #594 usage limits.';

COMMENT ON COLUMN openrails.catalog_meters.kind IS 'counter = summed events; gauge = time-integrated level samples.';

COMMENT ON COLUMN openrails.catalog_meters.event_type IS '#638 usage event type for rate-card meters; defaults to key when omitted.';

COMMENT ON COLUMN openrails.catalog_meters.value_property IS '#638 JSON/dimension property carrying the numeric quantity to aggregate.';

COMMENT ON COLUMN openrails.catalog_meters.aggregation IS '#638 aggregation mode for rate-card meters.';

COMMENT ON COLUMN openrails.catalog_meters.group_by IS '#638 dimension name -> event metadata/dimension property mapping for matrix pricing.';

ALTER TABLE ONLY openrails.catalog_meters
    ADD CONSTRAINT catalog_meters_pkey PRIMARY KEY (merchant_id, key);

ALTER TABLE ONLY openrails.catalog_meters
    ADD CONSTRAINT catalog_meters_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.catalog_meters ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.catalog_meters USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_meters TO openrails_app;

CREATE TABLE openrails.catalog_rate_cards (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    ordinal integer NOT NULL,
    meter_key text,
    payment_term text DEFAULT 'in_arrears'::text NOT NULL,
    filter jsonb DEFAULT '{}'::jsonb NOT NULL,
    allowance jsonb,
    price jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT catalog_rate_cards_ordinal_positive CHECK ((ordinal >= 1)),
    CONSTRAINT catalog_rate_cards_payment_term_check CHECK ((payment_term = ANY (ARRAY['in_advance'::text, 'in_arrears'::text])))
);

ALTER TABLE ONLY openrails.catalog_rate_cards FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.catalog_rate_cards IS '#638 rate-card sidecars: product usage/flat prices expressed as shared charge-model JSON. The ONLY metered-pricing engine (#707): legacy manifest metered: price declarations are translated into rate-card rows at push time (catalog_price_metered is gone).';

ALTER TABLE ONLY openrails.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_meter_fk FOREIGN KEY (merchant_id, meter_key) REFERENCES openrails.catalog_meters(merchant_id, key) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;

CREATE UNIQUE INDEX uq_catalog_rate_cards_meter ON openrails.catalog_rate_cards USING btree (merchant_id, meter_key) WHERE (meter_key IS NOT NULL);

CREATE UNIQUE INDEX uq_catalog_rate_cards_product_ordinal ON openrails.catalog_rate_cards USING btree (merchant_id, product_id, ordinal);

ALTER TABLE openrails.catalog_rate_cards ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.catalog_rate_cards USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_rate_cards TO openrails_app;

CREATE TABLE openrails.catalog_usage_limits (
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    measure text NOT NULL,
    windows jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT catalog_usage_limits_key_nonempty CHECK ((btrim(key) <> ''::text)),
    CONSTRAINT catalog_usage_limits_measure_nonempty CHECK ((btrim(measure) <> ''::text)),
    CONSTRAINT catalog_usage_limits_windows_array CHECK ((jsonb_typeof(windows) = 'array'::text))
);

ALTER TABLE ONLY openrails.catalog_usage_limits FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.catalog_usage_limits IS '#594 catalog usage-limit registry. Durable config only; Redis/Garnet owns request-time counters.';

COMMENT ON COLUMN openrails.catalog_usage_limits.measure IS 'Host-reported event stream key composed into admission policy; not a money meter.';

ALTER TABLE ONLY openrails.catalog_usage_limits
    ADD CONSTRAINT catalog_usage_limits_pkey PRIMARY KEY (merchant_id, key);

ALTER TABLE ONLY openrails.catalog_usage_limits
    ADD CONSTRAINT catalog_usage_limits_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.catalog_usage_limits ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.catalog_usage_limits USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_usage_limits TO openrails_app;

CREATE TABLE openrails.customers (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    issuer text,
    subject text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.customers FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.customers IS 'OpenRails payable identity. Customer identity is merchant_id plus the host/AuthKit stable UUID subject; id is that payable UUID. issuer is audit/last-seen source only.';

COMMENT ON COLUMN openrails.customers.issuer IS 'Audit/last-seen source issuer for delegated/remote customer touches. Not part of customer identity.';

COMMENT ON COLUMN openrails.customers.subject IS 'Host/AuthKit stable UUID subject. Natural key is (merchant_id, subject); issuer does not participate.';

ALTER TABLE ONLY openrails.customers
    ADD CONSTRAINT customers_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.customers
    ADD CONSTRAINT customers_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id);

CREATE INDEX idx_customers_merchant ON openrails.customers USING btree (merchant_id);

CREATE UNIQUE INDEX uq_customers_merchant_subject ON openrails.customers USING btree (merchant_id, subject) WHERE (subject IS NOT NULL);

ALTER TABLE openrails.customers ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.customers USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.customers TO openrails_app;

CREATE TABLE openrails.prices (
    id uuid DEFAULT uuidv7() NOT NULL,
    product_id uuid NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    rails jsonb,
    archived boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    access_duration_hours integer,
    auto_renew boolean DEFAULT false NOT NULL,
    trial_unit_amount bigint,
    trial_duration_hours integer,
    key text NOT NULL,
    CONSTRAINT prices_access_duration_positive_chk CHECK (((access_duration_hours IS NULL) OR (access_duration_hours > 0))),
    CONSTRAINT prices_amount_nonneg_chk CHECK ((amount >= 0)),
    CONSTRAINT prices_auto_renew_needs_duration_chk CHECK (((NOT auto_renew) OR (access_duration_hours IS NOT NULL))),
    CONSTRAINT prices_trial_amount_nonneg_chk CHECK (((trial_unit_amount IS NULL) OR (trial_unit_amount >= 0))),
    CONSTRAINT prices_trial_both_or_neither_chk CHECK (((trial_unit_amount IS NULL) = (trial_duration_hours IS NULL))),
    CONSTRAINT prices_trial_needs_auto_renew_chk CHECK (((trial_unit_amount IS NULL) OR auto_renew)),
    CONSTRAINT prices_trial_period_positive_chk CHECK (((trial_duration_hours IS NULL) OR (trial_duration_hours > 0)))
);

ALTER TABLE ONLY openrails.prices FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.prices IS 'Pricing tiers for products with rail-specific identifiers';

COMMENT ON COLUMN openrails.prices.amount IS 'Price amount in row currency micros (1 major unit = 1,000,000).';

COMMENT ON COLUMN openrails.prices.access_duration_hours IS 'access window in HOURS a purchase grants; NULL = indefinite/durable. For auto_renew, hours/24 is the provider billing cadence in days.';

COMMENT ON COLUMN openrails.prices.auto_renew IS '#622 whether the price recharges and extends the window after access_duration_hours (recurring).';

COMMENT ON COLUMN openrails.prices.trial_unit_amount IS '#622 optional first-phase price (micros); 0 = free trial; NULL = no trial.';

COMMENT ON COLUMN openrails.prices.trial_duration_hours IS 'optional trial first-phase length in HOURS; NULL = no trial.';

COMMENT ON COLUMN openrails.prices.key IS '#774: durable per-merchant-unique handle for this price''s substance-version chain. Immutable identity-wise (the row''s id is still the #662 substance UUID) but the LABEL can be relabeled in place (a key rename). At most one non-archived row per (merchant_id, key) — see uq_prices_merchant_key_current. Archived rows keep their key as a back-reference to the chain.';

ALTER TABLE ONLY openrails.prices
    ADD CONSTRAINT prices_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.prices
    ADD CONSTRAINT unique_prices_product_amount_window UNIQUE NULLS NOT DISTINCT (product_id, amount, currency, access_duration_hours, auto_renew, trial_unit_amount, trial_duration_hours);

ALTER TABLE ONLY openrails.prices
    ADD CONSTRAINT prices_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.prices
    ADD CONSTRAINT prices_product_id_fkey FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE RESTRICT;

CREATE INDEX idx_prices_archived ON openrails.prices USING btree (archived);

CREATE INDEX idx_prices_merchant_id ON openrails.prices USING btree (merchant_id);

CREATE INDEX idx_prices_merchant_key ON openrails.prices USING btree (merchant_id, key);

CREATE INDEX idx_prices_product_id ON openrails.prices USING btree (product_id);

CREATE INDEX idx_prices_rails ON openrails.prices USING gin (rails);

CREATE UNIQUE INDEX uq_prices_id_product_merchant ON openrails.prices USING btree (id, product_id, merchant_id);

CREATE UNIQUE INDEX uq_prices_merchant_key_current ON openrails.prices USING btree (merchant_id, key) WHERE (NOT archived);

CREATE TRIGGER trg_prices_default_key BEFORE INSERT ON openrails.prices FOR EACH ROW EXECUTE FUNCTION openrails.prices_default_key();

ALTER TABLE openrails.prices ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.prices USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.prices TO openrails_app;

CREATE TABLE openrails.rail_merchant_accounts (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    environment text DEFAULT 'live'::text NOT NULL,
    account_id text NOT NULL,
    display_name text,
    evidence jsonb,
    first_seen_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    last_verified_at timestamp with time zone,
    replaced_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    archived boolean DEFAULT false NOT NULL,
    CONSTRAINT rail_merchant_accounts_environment_check CHECK ((environment = ANY (ARRAY['live'::text, 'test'::text]))),
    CONSTRAINT rail_merchant_accounts_nonempty CHECK (((btrim(rail) <> ''::text) AND (btrim(environment) <> ''::text) AND (btrim(account_id) <> ''::text)))
);

ALTER TABLE ONLY openrails.rail_merchant_accounts FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.rail_merchant_accounts IS 'Merchant payment-provider account registry. A row is one merchant-owned account on one rail.';

COMMENT ON COLUMN openrails.rail_merchant_accounts.rail IS 'Payment rail/backend such as stripe, nmi, ccbill, solana, or a future rail.';

COMMENT ON COLUMN openrails.rail_merchant_accounts.environment IS 'Provider environment: live or test. Live and test accounts are distinct identities and may each have their own primary.';

COMMENT ON COLUMN openrails.rail_merchant_accounts.account_id IS 'Provider-returned account identity, e.g. Stripe acct_..., NMI profile account id, CCBill account/subaccount, or Solana authority address.';

COMMENT ON COLUMN openrails.rail_merchant_accounts.archived IS 'Drain-only provider-account lifecycle flag. false means eligible for new work; true remains addressable for existing obligations and inbound provider events.';

ALTER TABLE ONLY openrails.rail_merchant_accounts
    ADD CONSTRAINT rail_merchant_accounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.rail_merchant_accounts
    ADD CONSTRAINT rail_merchant_accounts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

CREATE INDEX idx_rail_merchant_accounts_merchant ON openrails.rail_merchant_accounts USING btree (merchant_id);

CREATE INDEX idx_rail_merchant_accounts_new_work ON openrails.rail_merchant_accounts USING btree (merchant_id, rail, environment, created_at DESC, id DESC) WHERE (archived = false);

CREATE UNIQUE INDEX uq_rail_merchant_accounts_identity ON openrails.rail_merchant_accounts USING btree (rail, environment, account_id);

ALTER TABLE openrails.rail_merchant_accounts ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.rail_merchant_accounts USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.rail_merchant_accounts TO openrails_app;

CREATE TABLE openrails.payment_methods (
    id uuid DEFAULT uuidv7() NOT NULL,
    rail character varying(50) NOT NULL,
    initial_transaction_id character varying(255) NOT NULL,
    last_four character varying(4),
    card_type character varying(50),
    expiry_date character varying(5),
    metadata jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    rail_merchant_account_id uuid,
    rail_customer_ref text DEFAULT ''::text NOT NULL,
    rail_method_ref text DEFAULT ''::text NOT NULL,
    rebill_driver text DEFAULT 'provider'::text NOT NULL,
    stored_credential_recurring_ref text DEFAULT ''::text NOT NULL,
    stored_credential_unscheduled_ref text DEFAULT ''::text NOT NULL,
    CONSTRAINT payment_methods_rebill_driver_check CHECK ((rebill_driver = ANY (ARRAY['provider'::text, 'openrails'::text])))
);

ALTER TABLE ONLY openrails.payment_methods FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.payment_methods IS 'Generalized payment method table supporting multiple rails.';

COMMENT ON COLUMN openrails.payment_methods.rail IS 'Payment rail type: nmi, ccbill, stripe, etc.';

COMMENT ON COLUMN openrails.payment_methods.rail_merchant_account_id IS 'Provider account that produced this vaulted payment method mirror row.';

COMMENT ON COLUMN openrails.payment_methods.rail_customer_ref IS 'Customer-scope rail handle (e.g. NMI customer_vault_id); '''' when the customer scope lives in rail_customers (Stripe).';

COMMENT ON COLUMN openrails.payment_methods.rail_method_ref IS 'Instrument-scope rail handle (e.g. NMI billing_id, Stripe pm_, Spreedly/HyperSwitch token).';

COMMENT ON COLUMN openrails.payment_methods.stored_credential_recurring_ref IS 'Rail-scoped stored-credential replay reference for the RECURRING card-network agreement (NMI: gateway transactionid of the initial recurring CIT, replayed as initial_transaction_id on recurring MITs). Empty = not captured yet.';

COMMENT ON COLUMN openrails.payment_methods.stored_credential_unscheduled_ref IS 'Rail-scoped stored-credential replay reference for the UNSCHEDULED card-network agreement (NMI: gateway transactionid of the initial unscheduled CIT, replayed as initial_transaction_id on unscheduled MITs). Empty = not captured yet.';

ALTER TABLE ONLY openrails.payment_methods
    ADD CONSTRAINT payment_methods_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.payment_methods
    ADD CONSTRAINT payment_methods_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.payment_methods
    ADD CONSTRAINT payment_methods_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.payment_methods
    ADD CONSTRAINT payment_methods_rail_merchant_account_fk FOREIGN KEY (rail_merchant_account_id) REFERENCES openrails.rail_merchant_accounts(id);

CREATE INDEX idx_payment_methods_customer ON openrails.payment_methods USING btree (customer_id) WHERE (customer_id IS NOT NULL);

CREATE INDEX idx_payment_methods_merchant_id ON openrails.payment_methods USING btree (merchant_id);

CREATE INDEX idx_payment_methods_method_ref ON openrails.payment_methods USING btree (rail, rail_method_ref);

CREATE INDEX idx_payment_methods_rail ON openrails.payment_methods USING btree (rail);

CREATE INDEX idx_payment_methods_rail_merchant_account ON openrails.payment_methods USING btree (rail_merchant_account_id) WHERE (rail_merchant_account_id IS NOT NULL);

CREATE UNIQUE INDEX uq_payment_methods_customer_instrument_legacy ON openrails.payment_methods USING btree (merchant_id, customer_id, rail_customer_ref, rail_method_ref) WHERE (rail_merchant_account_id IS NULL);

CREATE UNIQUE INDEX uq_payment_methods_merchant_rail_instrument_legacy ON openrails.payment_methods USING btree (merchant_id, rail, rail_customer_ref, rail_method_ref) WHERE (rail_merchant_account_id IS NULL);

CREATE UNIQUE INDEX uq_payment_methods_rail_merchant_account_instrument ON openrails.payment_methods USING btree (merchant_id, rail_merchant_account_id, rail_customer_ref, rail_method_ref) WHERE (rail_merchant_account_id IS NOT NULL);

ALTER TABLE openrails.payment_methods ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.payment_methods USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payment_methods TO openrails_app;

CREATE TABLE openrails.subscriptions (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid,
    product_id uuid NOT NULL,
    status openrails.subscription_status DEFAULT 'pending'::openrails.subscription_status NOT NULL,
    rail text NOT NULL,
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
    rail_merchant_account_id uuid,
    CONSTRAINT chk_cancelled_has_timestamp CHECK (((status <> 'cancelled'::openrails.subscription_status) OR (cancelled_at IS NOT NULL))),
    CONSTRAINT chk_cancelled_has_type CHECK (((status <> 'cancelled'::openrails.subscription_status) OR (cancel_type IS NOT NULL))),
    CONSTRAINT chk_cancelled_no_retry_schedule CHECK (((status <> 'cancelled'::openrails.subscription_status) OR ((next_retry_at IS NULL) AND (grace_ends_at IS NULL)))),
    CONSTRAINT chk_ended_not_before_cancelled CHECK (((ended_at IS NULL) OR (cancelled_at IS NULL) OR (ended_at >= cancelled_at))),
    CONSTRAINT chk_past_due_has_period_end CHECK (((status <> 'past_due'::openrails.subscription_status) OR (current_period_ends_at IS NOT NULL))),
    CONSTRAINT chk_valid_period CHECK (((current_period_starts_at IS NULL) OR (current_period_ends_at IS NULL) OR (current_period_starts_at < current_period_ends_at)))
);

ALTER TABLE ONLY openrails.subscriptions FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.subscriptions IS 'Core subscription records tracking user billing relationships';

COMMENT ON COLUMN openrails.subscriptions.product_id IS 'Denormalized product ID for efficient user+product lookups without joining prices';

COMMENT ON COLUMN openrails.subscriptions.scheduled_price_id IS 'Price ID for scheduled tier change (downgrade). Applied at end of current billing period during renewal.';

COMMENT ON COLUMN openrails.subscriptions.tier_group IS 'Denormalized from openrails.products.tier_group (kept in sync by trigger trg_subscriptions_set_tier_group). Backs uq_subscriptions_customer_tier_group_active, which enforces one active/pending subscription per (customer, tier group).';

COMMENT ON COLUMN openrails.subscriptions.rail_merchant_account_id IS 'Provider account that produced this remote subscription mirror row.';

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_payment_method_id_fkey FOREIGN KEY (payment_method_id) REFERENCES openrails.payment_methods(id) ON DELETE SET NULL;

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_price_id_fkey FOREIGN KEY (price_id) REFERENCES openrails.prices(id);

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_price_product_merchant_fkey FOREIGN KEY (price_id, product_id, merchant_id) REFERENCES openrails.prices(id, product_id, merchant_id);

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_product_id_fkey FOREIGN KEY (product_id) REFERENCES openrails.products(id);

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_rail_merchant_account_fk FOREIGN KEY (rail_merchant_account_id) REFERENCES openrails.rail_merchant_accounts(id);

ALTER TABLE ONLY openrails.subscriptions
    ADD CONSTRAINT subscriptions_scheduled_price_id_fkey FOREIGN KEY (scheduled_price_id) REFERENCES openrails.prices(id);

CREATE INDEX idx_subscriptions_customer ON openrails.subscriptions USING btree (customer_id) WHERE (customer_id IS NOT NULL);

CREATE INDEX idx_subscriptions_customer_active_created ON openrails.subscriptions USING btree (customer_id, created_at DESC) WHERE (status = 'active'::openrails.subscription_status);

CREATE INDEX idx_subscriptions_due_dunning ON openrails.subscriptions USING btree (next_retry_at, rail) WHERE ((status = 'past_due'::openrails.subscription_status) AND (next_retry_at IS NOT NULL));

CREATE INDEX idx_subscriptions_gateway_order_id ON openrails.subscriptions USING btree (merchant_id, rail, ((gateway_response ->> 'order_id'::text))) WHERE ((gateway_response ->> 'order_id'::text) IS NOT NULL);

CREATE INDEX idx_subscriptions_grace_ends_at ON openrails.subscriptions USING btree (grace_ends_at) WHERE (grace_ends_at IS NOT NULL);

CREATE INDEX idx_subscriptions_merchant_cancelled ON openrails.subscriptions USING btree (merchant_id, cancelled_at) WHERE (cancelled_at IS NOT NULL);

CREATE INDEX idx_subscriptions_merchant_ended ON openrails.subscriptions USING btree (merchant_id, ended_at) WHERE (ended_at IS NOT NULL);

CREATE INDEX idx_subscriptions_merchant_id ON openrails.subscriptions USING btree (merchant_id);

CREATE INDEX idx_subscriptions_merchant_started ON openrails.subscriptions USING btree (merchant_id, started_at);

CREATE INDEX idx_subscriptions_next_retry_at ON openrails.subscriptions USING btree (next_retry_at) WHERE (next_retry_at IS NOT NULL);

CREATE INDEX idx_subscriptions_payment_method_id ON openrails.subscriptions USING btree (payment_method_id);

CREATE INDEX idx_subscriptions_period_overdue ON openrails.subscriptions USING btree (current_period_ends_at) WHERE (status = 'active'::openrails.subscription_status);

CREATE INDEX idx_subscriptions_price_id ON openrails.subscriptions USING btree (price_id);

CREATE INDEX idx_subscriptions_product_id ON openrails.subscriptions USING btree (product_id);

CREATE INDEX idx_subscriptions_rail ON openrails.subscriptions USING btree (rail);

CREATE INDEX idx_subscriptions_rail_merchant_account ON openrails.subscriptions USING btree (rail_merchant_account_id) WHERE (rail_merchant_account_id IS NOT NULL);

CREATE INDEX idx_subscriptions_rail_subscription ON openrails.subscriptions USING btree (rail, rail_subscription_id);

CREATE INDEX idx_subscriptions_status ON openrails.subscriptions USING btree (status);

CREATE UNIQUE INDEX uq_subscriptions_customer_product_lifecycle ON openrails.subscriptions USING btree (merchant_id, customer_id, product_id) WHERE (status = ANY (ARRAY['active'::openrails.subscription_status, 'pending'::openrails.subscription_status, 'past_due'::openrails.subscription_status]));

CREATE UNIQUE INDEX uq_subscriptions_customer_tier_group_active ON openrails.subscriptions USING btree (customer_id, tier_group) WHERE ((status = ANY (ARRAY['active'::openrails.subscription_status, 'pending'::openrails.subscription_status])) AND (tier_group IS NOT NULL));

CREATE UNIQUE INDEX uq_subscriptions_merchant_rail_subscription_id_legacy ON openrails.subscriptions USING btree (merchant_id, rail, rail_subscription_id) WHERE ((rail_merchant_account_id IS NULL) AND (rail_subscription_id <> ''::text));

CREATE UNIQUE INDEX uq_subscriptions_rail_merchant_account_subscription ON openrails.subscriptions USING btree (merchant_id, rail_merchant_account_id, rail_subscription_id) WHERE ((rail_merchant_account_id IS NOT NULL) AND (rail_subscription_id <> ''::text));

CREATE TRIGGER trg_subscriptions_set_tier_group BEFORE INSERT OR UPDATE OF product_id ON openrails.subscriptions FOR EACH ROW EXECUTE FUNCTION openrails.subscriptions_set_tier_group();

CREATE TRIGGER trg_subscriptions_status_transition AFTER INSERT OR UPDATE OF status ON openrails.subscriptions FOR EACH ROW EXECUTE FUNCTION openrails.subscriptions_record_status_transition();

ALTER TABLE openrails.subscriptions ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.subscriptions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.subscriptions TO openrails_app;

CREATE TABLE openrails.payments (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid NOT NULL,
    rail text NOT NULL,
    transaction_id text NOT NULL,
    amount bigint NOT NULL,
    list_amount bigint NOT NULL,
    currency text NOT NULL,
    status openrails.payment_status DEFAULT 'completed'::openrails.payment_status NOT NULL,
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
    rail_merchant_account_id uuid,
    attempt_kind text,
    failure_code text,
    failure_reason text,
    reversal_kind text,
    CONSTRAINT chk_payment_not_future CHECK ((purchased_at <= (now() + '00:05:00'::interval))),
    CONSTRAINT chk_payments_attempt_kind CHECK (((attempt_kind IS NULL) OR (attempt_kind = ANY (ARRAY['initial'::text, 'renewal'::text])))),
    CONSTRAINT chk_payments_reversal_kind CHECK (((reversal_kind IS NULL) OR (reversal_kind = ANY (ARRAY['refund'::text, 'chargeback'::text, 'dispute_reversal'::text]))))
);

ALTER TABLE ONLY openrails.payments FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.payments IS 'Records of all payment transactions (formerly purchases table)';

COMMENT ON COLUMN openrails.payments.subscription_id IS 'Links a payment to the subscription that generated it (nullable for one-off payments)';

COMMENT ON COLUMN openrails.payments.rail_merchant_account_id IS 'Provider account that produced this payment/charge mirror row.';

COMMENT ON COLUMN openrails.payments.attempt_kind IS '#733 initial|renewal, stamped at write time by the checkout vs rebill paths; NULL = unknown (imported/pre-instrumentation rows).';

COMMENT ON COLUMN openrails.payments.failure_code IS '#733 raw rail decline code, recorded verbatim (no fabrication).';

COMMENT ON COLUMN openrails.payments.failure_reason IS '#733 normalized decline category, derived deterministically from failure_code per rail.';

COMMENT ON COLUMN openrails.payments.reversal_kind IS '#733 discriminates mirror rows: refund | chargeback | dispute_reversal (dispute won). NULL on sale rows.';

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_price_id_fkey FOREIGN KEY (price_id) REFERENCES openrails.prices(id);

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_rail_merchant_account_fk FOREIGN KEY (rail_merchant_account_id) REFERENCES openrails.rail_merchant_accounts(id);

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_refunded_payment_id_fkey FOREIGN KEY (refunded_payment_id) REFERENCES openrails.payments(id);

ALTER TABLE ONLY openrails.payments
    ADD CONSTRAINT payments_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE SET NULL;

CREATE INDEX idx_payments_customer ON openrails.payments USING btree (customer_id) WHERE (customer_id IS NOT NULL);

CREATE INDEX idx_payments_merchant_created ON openrails.payments USING btree (merchant_id, created_at DESC);

CREATE INDEX idx_payments_merchant_id ON openrails.payments USING btree (merchant_id);

CREATE INDEX idx_payments_merchant_purchased ON openrails.payments USING btree (merchant_id, purchased_at);

CREATE INDEX idx_payments_merchant_rail_transaction ON openrails.payments USING btree (merchant_id, rail, transaction_id);

CREATE INDEX idx_payments_metadata_nmi_order ON openrails.payments USING btree (merchant_id, ((metadata ->> 'nmi_subscription_order_id'::text))) WHERE ((metadata ->> 'nmi_subscription_order_id'::text) IS NOT NULL);

CREATE INDEX idx_payments_metadata_stripe_invoice ON openrails.payments USING btree (merchant_id, ((metadata ->> 'stripe_invoice_id'::text))) WHERE ((metadata ->> 'stripe_invoice_id'::text) IS NOT NULL);

CREATE INDEX idx_payments_price_id ON openrails.payments USING btree (price_id);

CREATE INDEX idx_payments_purchased_at ON openrails.payments USING btree (purchased_at);

CREATE INDEX idx_payments_rail ON openrails.payments USING btree (rail);

CREATE INDEX idx_payments_rail_merchant_account ON openrails.payments USING btree (rail_merchant_account_id) WHERE (rail_merchant_account_id IS NOT NULL);

CREATE INDEX idx_payments_refunded_payment_id ON openrails.payments USING btree (refunded_payment_id);

CREATE INDEX idx_payments_subscription_id ON openrails.payments USING btree (subscription_id);

CREATE UNIQUE INDEX uq_payments_merchant_rail_transaction_legacy ON openrails.payments USING btree (merchant_id, rail, transaction_id) WHERE (rail_merchant_account_id IS NULL);

CREATE UNIQUE INDEX uq_payments_rail_merchant_account_transaction ON openrails.payments USING btree (merchant_id, rail_merchant_account_id, transaction_id) WHERE (rail_merchant_account_id IS NOT NULL);

ALTER TABLE openrails.payments ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.payments USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payments TO openrails_app;

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
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    rail_merchant_account_id uuid,
    CONSTRAINT checkout_sessions_mode_check CHECK ((mode = ANY (ARRAY['one_off'::text, 'subscription'::text])))
);

ALTER TABLE ONLY openrails.checkout_sessions FORCE ROW LEVEL SECURITY;

COMMENT ON COLUMN openrails.checkout_sessions.rail_merchant_account_id IS 'Provider account selected for this provider checkout/session.';

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_payment_id_fkey FOREIGN KEY (payment_id) REFERENCES openrails.payments(id);

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_price_id_fkey FOREIGN KEY (price_id) REFERENCES openrails.prices(id);

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_rail_merchant_account_fk FOREIGN KEY (rail_merchant_account_id) REFERENCES openrails.rail_merchant_accounts(id);

ALTER TABLE ONLY openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id);

CREATE INDEX checkout_sessions_expires_at_idx ON openrails.checkout_sessions USING btree (expires_at);

CREATE UNIQUE INDEX checkout_sessions_rail_reference_idx ON openrails.checkout_sessions USING btree (rail, reference) WHERE (reference IS NOT NULL);

CREATE UNIQUE INDEX checkout_sessions_rail_transaction_id_idx ON openrails.checkout_sessions USING btree (rail, transaction_id) WHERE (transaction_id IS NOT NULL);

CREATE INDEX idx_checkout_sessions_customer ON openrails.checkout_sessions USING btree (customer_id) WHERE (customer_id IS NOT NULL);

CREATE INDEX idx_checkout_sessions_merchant_id ON openrails.checkout_sessions USING btree (merchant_id);

CREATE INDEX idx_checkout_sessions_rail_merchant_account ON openrails.checkout_sessions USING btree (rail_merchant_account_id) WHERE (rail_merchant_account_id IS NOT NULL);

ALTER TABLE openrails.checkout_sessions ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.checkout_sessions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.checkout_sessions TO openrails_app;

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

COMMENT ON TABLE openrails.custom_credit_types IS 'Per-tenant custom credit units (#475): consume-only, no FX, never billed in. Referenced from money rows via the qualified code tenant-slug/name. Written by the catalog sidecar push (#706): auto-defined from catalog_credit_balances.unit; never auto-deactivated (grants may still reference a removed balance''s unit).';

COMMENT ON COLUMN openrails.custom_credit_types.decimals IS 'Minor-unit scale for presentation (10^decimals minor units per major unit). Storage is always integer minor units.';

ALTER TABLE ONLY openrails.custom_credit_types
    ADD CONSTRAINT custom_credit_types_merchant_name_key UNIQUE (merchant_id, name);

ALTER TABLE ONLY openrails.custom_credit_types
    ADD CONSTRAINT custom_credit_types_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.custom_credit_types
    ADD CONSTRAINT custom_credit_types_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_custom_credit_types_merchant_id ON openrails.custom_credit_types USING btree (merchant_id);

ALTER TABLE openrails.custom_credit_types ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.custom_credit_types USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.custom_credit_types TO openrails_app;

CREATE TABLE openrails.customer_minimum_spend (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    amount_micros bigint NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT customer_minimum_spend_amount_positive CHECK ((amount_micros > 0))
);

ALTER TABLE ONLY openrails.customer_minimum_spend FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.customer_minimum_spend IS '#643 per-customer per-currency minimum-spend commitment; trues-up at periodic invoice close.';

ALTER TABLE ONLY openrails.customer_minimum_spend
    ADD CONSTRAINT customer_minimum_spend_pkey PRIMARY KEY (merchant_id, customer_id, currency);

ALTER TABLE ONLY openrails.customer_minimum_spend
    ADD CONSTRAINT customer_minimum_spend_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE CASCADE;

ALTER TABLE ONLY openrails.customer_minimum_spend
    ADD CONSTRAINT customer_minimum_spend_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.customer_minimum_spend ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.customer_minimum_spend USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.customer_minimum_spend TO openrails_app;

CREATE TABLE openrails.dashboard_configs (
    merchant_id uuid NOT NULL,
    layout jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_by text
);

ALTER TABLE ONLY openrails.dashboard_configs FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.dashboard_configs IS '#741 per-merchant dashboard widget layout: [{id, title, viz(stat|line|area|bar|donut|table), query(#733 body), grid{x,y,w,h}}]. Absent row = seeded default template (in code, not DB).';

COMMENT ON COLUMN openrails.dashboard_configs.updated_by IS 'acting principal (user id) of the last PUT; informational.';

ALTER TABLE ONLY openrails.dashboard_configs
    ADD CONSTRAINT dashboard_configs_pkey PRIMARY KEY (merchant_id);

ALTER TABLE ONLY openrails.dashboard_configs
    ADD CONSTRAINT dashboard_configs_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.dashboard_configs ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.dashboard_configs USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.dashboard_configs TO openrails_app;

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
    CONSTRAINT grants_termination_no_window CHECK (((event = 'grant'::text) OR (ends_at IS NULL))),
    CONSTRAINT grants_valid_window CHECK (((ends_at IS NULL) OR (starts_at < ends_at)))
);

ALTER TABLE ONLY openrails.grants FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.grants IS '#514 append-only grant ledger: the access-domain sibling of the #512 money ledger. Immutable events (grant/revoke/expire/supersede/adjust); the live entitlement windows, product ownership, and credit lots are DERIVED projections folded from this log. A credit grant carries the lot amount+currency and IS the FIFO credit lot (subsumes the old money_blocks role); derive-2 emits its #512 deposit transfer tagged source=grant.';

COMMENT ON COLUMN openrails.grants.event IS 'grant roots a grant; revoke/expire/supersede/adjust are new rows referencing it via supersedes_id. The grant row is never updated.';

COMMENT ON COLUMN openrails.grants.spec_snapshot IS 'Product entitlements/credits spec captured at issuance so derive-2 (grant->projection) is a pure function and replay is exact + historical.';

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_pkey PRIMARY KEY (id);

COMMENT ON CONSTRAINT grants_termination_no_window ON openrails.grants IS 'Only grant events carry an access window; revoke/expire/supersede are window-less point events (valid-time instant on starts_at, transaction time on created_at).';

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_payment_fk FOREIGN KEY (payment_id) REFERENCES openrails.payments(id);

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id);

ALTER TABLE ONLY openrails.grants
    ADD CONSTRAINT grants_supersedes_fk FOREIGN KEY (supersedes_id) REFERENCES openrails.grants(id);

CREATE INDEX idx_grants_credit_expiry ON openrails.grants USING btree (merchant_id, ends_at) WHERE ((kind = 'credit'::text) AND (event = 'grant'::text) AND (ends_at IS NOT NULL));

CREATE INDEX idx_grants_customer ON openrails.grants USING btree (merchant_id, customer_id);

CREATE INDEX idx_grants_customer_kind ON openrails.grants USING btree (merchant_id, customer_id, kind) WHERE (event = 'grant'::text);

CREATE INDEX idx_grants_merchant_credit_created ON openrails.grants USING btree (merchant_id, created_at) WHERE (kind = 'credit'::text);

CREATE INDEX idx_grants_merchant_id ON openrails.grants USING btree (merchant_id);

CREATE INDEX idx_grants_source ON openrails.grants USING btree (merchant_id, source_type, source_id) WHERE (source_id <> ''::text);

CREATE INDEX idx_grants_supersedes ON openrails.grants USING btree (supersedes_id) WHERE (supersedes_id IS NOT NULL);

CREATE UNIQUE INDEX uq_grants_termination ON openrails.grants USING btree (supersedes_id) WHERE ((supersedes_id IS NOT NULL) AND (event = ANY (ARRAY['revoke'::text, 'expire'::text, 'supersede'::text])));

ALTER TABLE openrails.grants ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.grants USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT ON TABLE openrails.grants TO openrails_app;

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

COMMENT ON COLUMN openrails.entitlements.customer_id IS 'OpenRails payable tenant subject for this entitlement window.';

ALTER TABLE ONLY openrails.entitlements
    ADD CONSTRAINT entitlements_customer_no_overlap EXCLUDE USING gist (merchant_id WITH =, customer_id WITH =, entitlement WITH =, period WITH &&) WHERE (((customer_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL)));

ALTER TABLE ONLY openrails.entitlements
    ADD CONSTRAINT entitlements_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.entitlements
    ADD CONSTRAINT entitlements_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.entitlements
    ADD CONSTRAINT entitlements_grant_fk FOREIGN KEY (grant_id) REFERENCES openrails.grants(id);

ALTER TABLE ONLY openrails.entitlements
    ADD CONSTRAINT entitlements_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_entitlements_closed_at ON openrails.entitlements USING btree (merchant_id, LEAST(COALESCE(end_at, 'infinity'::timestamp with time zone), COALESCE(revoked_at, 'infinity'::timestamp with time zone))) WHERE ((end_at IS NOT NULL) OR (revoked_at IS NOT NULL));

CREATE INDEX idx_entitlements_customer_active_window ON openrails.entitlements USING btree (merchant_id, customer_id, entitlement, start_at, end_at) WHERE ((customer_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL));

CREATE INDEX idx_entitlements_grace_by_subscription_live ON openrails.entitlements USING btree (source_id, entitlement, start_at, end_at) WHERE ((source_type = 'grace'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));

CREATE INDEX idx_entitlements_grant_id ON openrails.entitlements USING btree (grant_id) WHERE (grant_id IS NOT NULL);

CREATE INDEX idx_entitlements_live_by_id ON openrails.entitlements USING btree (id) WHERE ((revoked_at IS NULL) AND (deleted_at IS NULL));

CREATE INDEX idx_entitlements_merchant_id ON openrails.entitlements USING btree (merchant_id);

CREATE INDEX idx_entitlements_one_off_source_live ON openrails.entitlements USING btree (source_id, entitlement) WHERE ((source_type = 'one_off'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));

CREATE INDEX idx_entitlements_reverse_active ON openrails.entitlements USING btree (merchant_id, entitlement, customer_id) WHERE ((revoked_at IS NULL) AND (deleted_at IS NULL));

CREATE INDEX idx_entitlements_source ON openrails.entitlements USING btree (source_type, source_id) WHERE (source_id IS NOT NULL);

CREATE INDEX idx_entitlements_subscription_source_live ON openrails.entitlements USING btree (source_id, entitlement, end_at) WHERE ((source_type = 'subscription'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));

CREATE UNIQUE INDEX uq_entitlements_customer_active ON openrails.entitlements USING btree (merchant_id, customer_id, entitlement) WHERE ((customer_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL) AND (end_at IS NULL));

ALTER TABLE openrails.entitlements ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.entitlements USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.entitlements TO openrails_app;

CREATE TABLE openrails.finding_digest_state (
    merchant_id uuid NOT NULL,
    last_digested_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.finding_digest_state FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.finding_digest_state IS '#787: one row per merchant recording when the low-severity reconciliation-findings digest last fired.';

ALTER TABLE ONLY openrails.finding_digest_state
    ADD CONSTRAINT finding_digest_state_pkey PRIMARY KEY (merchant_id);

ALTER TABLE ONLY openrails.finding_digest_state
    ADD CONSTRAINT finding_digest_state_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

ALTER TABLE openrails.finding_digest_state ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.finding_digest_state USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.finding_digest_state TO openrails_app;

CREATE TABLE openrails.imported_dunning_history (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    subscription_id uuid,
    customer_id uuid,
    event_type text NOT NULL,
    rail text NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    source text NOT NULL,
    detail jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.imported_dunning_history FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.imported_dunning_history IS 'Append-only imported legacy dunning history (#735; doujins #387 import target). Display/forensics evidence only.';

COMMENT ON COLUMN openrails.imported_dunning_history.source IS 'Legacy origin of the imported row, e.g. doujins_users_logs, mobius_schedulers.';

COMMENT ON COLUMN openrails.imported_dunning_history.detail IS 'Verbatim normalized legacy payload. Correlation keys the reconcile history source extracts when present: rail_subscription_id, rail_transaction_id, status, amount_micros.';

ALTER TABLE ONLY openrails.imported_dunning_history
    ADD CONSTRAINT imported_dunning_history_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.imported_dunning_history
    ADD CONSTRAINT imported_dunning_history_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE SET NULL;

ALTER TABLE ONLY openrails.imported_dunning_history
    ADD CONSTRAINT imported_dunning_history_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

ALTER TABLE ONLY openrails.imported_dunning_history
    ADD CONSTRAINT imported_dunning_history_subscription_fk FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE SET NULL;

CREATE INDEX idx_imported_dunning_history_merchant_occurred ON openrails.imported_dunning_history USING btree (merchant_id, occurred_at);

ALTER TABLE openrails.imported_dunning_history ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.imported_dunning_history USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.imported_dunning_history TO openrails_app;

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
    finalized_at timestamp with time zone,
    external_invoice_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT invoices_amounts_nonneg_chk CHECK (((subtotal_amount >= 0) AND (total_amount >= 0) AND (amount_paid >= 0) AND (amount_due >= 0))),
    CONSTRAINT invoices_collection_method_check CHECK ((collection_method = 'charge_automatically'::text)),
    CONSTRAINT invoices_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'open'::text, 'paid'::text, 'past_due'::text, 'voided'::text, 'uncollectible'::text, 'finalized'::text])))
);

ALTER TABLE ONLY openrails.invoices FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.invoices IS 'Period invoices/statements. For arrears, an open invoice is the receivable and payments are allocated to it. Prepaid invoices remain informational receipts/statements.';

COMMENT ON COLUMN openrails.invoices.amount_due IS 'Outstanding amount for this invoice in the row currency internal precision. Open arrears balance is derived from open/past-due invoices.';

COMMENT ON COLUMN openrails.invoices.line_items IS 'Immutable as-billed statement itemization frozen at close (#726): per-event_type usage rollups plus adjustment lines (e.g. minimum_spend_trueup). The only reader-facing line-item representation.';

ALTER TABLE ONLY openrails.invoices
    ADD CONSTRAINT invoices_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.invoices
    ADD CONSTRAINT invoices_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.invoices
    ADD CONSTRAINT invoices_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_invoices_customer ON openrails.invoices USING btree (customer_id, period_from DESC);

CREATE INDEX ix_invoices_open_due ON openrails.invoices USING btree (merchant_id, customer_id, currency, due_at) WHERE ((status = ANY (ARRAY['open'::text, 'past_due'::text])) AND (amount_due > 0));

CREATE INDEX ix_invoices_payer ON openrails.invoices USING btree (merchant_id, customer_id, period_from DESC);

CREATE UNIQUE INDEX uq_invoices_period ON openrails.invoices USING btree (merchant_id, customer_id, currency, period_from, period_to);

ALTER TABLE openrails.invoices ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.invoices USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoices TO openrails_app;

CREATE TABLE openrails.invoice_items (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    invoice_id uuid,
    source_type text NOT NULL,
    source_id text NOT NULL,
    invoice_at timestamp with time zone NOT NULL,
    amount bigint NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT invoice_items_amount_nonneg_chk CHECK ((amount >= 0)),
    CONSTRAINT invoice_items_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'invoiced'::text, 'voided'::text])))
);

ALTER TABLE ONLY openrails.invoice_items FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.invoice_items IS 'Pending-accrual workspace (#726): owed accruals queue as pending rows gating arrears exposure; finalization attaches them (invoice_id, status=invoiced) so they cannot bill twice. NOT the statement itemization — that is invoices.line_items.';

ALTER TABLE ONLY openrails.invoice_items
    ADD CONSTRAINT invoice_items_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.invoice_items
    ADD CONSTRAINT invoice_items_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.invoice_items
    ADD CONSTRAINT invoice_items_invoice_fk FOREIGN KEY (invoice_id) REFERENCES openrails.invoices(id) ON DELETE SET NULL;

ALTER TABLE ONLY openrails.invoice_items
    ADD CONSTRAINT invoice_items_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX ix_invoice_items_invoice ON openrails.invoice_items USING btree (merchant_id, invoice_id);

CREATE INDEX ix_invoice_items_pending ON openrails.invoice_items USING btree (merchant_id, customer_id, currency, invoice_at) WHERE ((invoice_id IS NULL) AND (status = 'pending'::text));

CREATE UNIQUE INDEX uq_invoice_items_source ON openrails.invoice_items USING btree (merchant_id, customer_id, currency, source_type, source_id);

ALTER TABLE openrails.invoice_items ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.invoice_items USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoice_items TO openrails_app;

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

COMMENT ON TABLE openrails.ledger_accounts IS '#512 double-entry ledger accounts. One account belongs to exactly one (merchant, currency) ledger; TB-style posted/pending counters are maintained from immutable ledger_transfers and verified by reconciliation. account_type identifies its role (customer_balance, platform_revenue, processor_clearing, arrears_liability, expired_credits, fx_liquidity, world).';

COMMENT ON COLUMN openrails.ledger_accounts.customer_id IS 'NULL for system accounts (one per merchant+currency); set for per-customer balance accounts.';

COMMENT ON COLUMN openrails.ledger_accounts.debits_must_not_exceed_credits IS 'TB sign flag: balance (credits-debits) may not go below zero (minus an applier-supplied arrears floor). Set on customer_balance.';

COMMENT ON COLUMN openrails.ledger_accounts.credits_posted IS 'Phase H maintained counter: posted credits for O(1) balance reads.';

COMMENT ON COLUMN openrails.ledger_accounts.debits_posted IS 'Phase H maintained counter: posted debits for O(1) balance reads.';

ALTER TABLE ONLY openrails.ledger_accounts
    ADD CONSTRAINT ledger_accounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.ledger_accounts
    ADD CONSTRAINT ledger_accounts_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.ledger_accounts
    ADD CONSTRAINT ledger_accounts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_ledger_accounts_customer ON openrails.ledger_accounts USING btree (customer_id) WHERE (customer_id IS NOT NULL);

CREATE INDEX idx_ledger_accounts_merchant_id ON openrails.ledger_accounts USING btree (merchant_id);

CREATE UNIQUE INDEX uq_ledger_accounts_customer ON openrails.ledger_accounts USING btree (merchant_id, customer_id, account_type, currency) WHERE (customer_id IS NOT NULL);

CREATE UNIQUE INDEX uq_ledger_accounts_system ON openrails.ledger_accounts USING btree (merchant_id, account_type, currency) WHERE (customer_id IS NULL);

ALTER TABLE openrails.ledger_accounts ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.ledger_accounts USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT ON TABLE openrails.ledger_accounts TO openrails_app;

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

COMMENT ON TABLE openrails.ledger_transfers IS '#512 immutable double-entry transfers. Append-only (role granted SELECT,INSERT only). A transfer moves amount debit->credit within ONE (merchant, currency) ledger; capture/void/refund/expiry are NEW rows, never updates. ledger_accounts counters are a maintained projection of this table.';

COMMENT ON COLUMN openrails.ledger_transfers.allow_debit_negative_up_to IS 'Debit-account floor used by the counter trigger for debits_must_not_exceed_credits accounts. Usually 0; arrears paths pass the current credit-line allowance.';

COMMENT ON COLUMN openrails.ledger_transfers.source IS 'Opaque origin key (e.g. ''grant''/grant_id, ''payment''/transaction_id). Ledger purity: business joins live in control-plane tables.';

COMMENT ON COLUMN openrails.ledger_transfers.grant_id IS 'Credit-lot attribution. grant_id/invoice_id/customer_id deliberately carry NO FKs (ledger purity, #709): the append-only ledger never blocks or cascades on control-plane rows.';

ALTER TABLE ONLY openrails.ledger_transfers
    ADD CONSTRAINT ledger_transfers_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.ledger_transfers
    ADD CONSTRAINT ledger_transfers_credit_fk FOREIGN KEY (credit_account_id) REFERENCES openrails.ledger_accounts(id);

ALTER TABLE ONLY openrails.ledger_transfers
    ADD CONSTRAINT ledger_transfers_debit_fk FOREIGN KEY (debit_account_id) REFERENCES openrails.ledger_accounts(id);

ALTER TABLE ONLY openrails.ledger_transfers
    ADD CONSTRAINT ledger_transfers_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_ledger_transfers_credit ON openrails.ledger_transfers USING btree (credit_account_id);

CREATE INDEX idx_ledger_transfers_customer ON openrails.ledger_transfers USING btree (merchant_id, customer_id, currency, created_at DESC) WHERE (customer_id IS NOT NULL);

CREATE INDEX idx_ledger_transfers_debit ON openrails.ledger_transfers USING btree (debit_account_id);

CREATE INDEX idx_ledger_transfers_grant ON openrails.ledger_transfers USING btree (merchant_id, grant_id) WHERE (grant_id IS NOT NULL);

CREATE UNIQUE INDEX idx_ledger_transfers_lot_once ON openrails.ledger_transfers USING btree (merchant_id, grant_id, transfer_type) WHERE ((grant_id IS NOT NULL) AND (transfer_type = ANY (ARRAY['deposit'::text, 'credit_expire'::text, 'credit_revoke'::text])));

CREATE INDEX idx_ledger_transfers_merchant_created ON openrails.ledger_transfers USING btree (merchant_id, created_at);

CREATE INDEX idx_ledger_transfers_merchant_id ON openrails.ledger_transfers USING btree (merchant_id);

CREATE UNIQUE INDEX idx_ledger_transfers_owed_accrual_once ON openrails.ledger_transfers USING btree (merchant_id, customer_id, currency, source, source_id) WHERE ((transfer_type = 'owed_accrual'::text) AND (source_id <> ''::text));

CREATE INDEX idx_ledger_transfers_source ON openrails.ledger_transfers USING btree (merchant_id, source, source_id) WHERE (source IS NOT NULL);

CREATE TRIGGER trg_ledger_transfers_apply_counters BEFORE INSERT ON openrails.ledger_transfers FOR EACH ROW EXECUTE FUNCTION openrails.ledger_transfers_apply_counters();

ALTER TABLE openrails.ledger_transfers ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.ledger_transfers USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT ON TABLE openrails.ledger_transfers TO openrails_app;

CREATE TABLE openrails.invoice_payments (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    invoice_id uuid NOT NULL,
    ledger_transfer_id uuid,
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
    rail_merchant_account_id uuid,
    CONSTRAINT invoice_payments_amount_positive_chk CHECK ((amount > 0)),
    CONSTRAINT invoice_payments_status_check CHECK ((status = ANY (ARRAY['attempted'::text, 'settled'::text, 'failed'::text])))
);

ALTER TABLE ONLY openrails.invoice_payments FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.invoice_payments IS 'Payment attempts and settled payments allocated to a specific invoice.';

COMMENT ON COLUMN openrails.invoice_payments.rail_merchant_account_id IS 'Provider account used for this invoice payment attempt or settled provider payment.';

ALTER TABLE ONLY openrails.invoice_payments
    ADD CONSTRAINT invoice_payments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.invoice_payments
    ADD CONSTRAINT invoice_payments_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.invoice_payments
    ADD CONSTRAINT invoice_payments_invoice_fk FOREIGN KEY (invoice_id) REFERENCES openrails.invoices(id) ON DELETE CASCADE;

ALTER TABLE ONLY openrails.invoice_payments
    ADD CONSTRAINT invoice_payments_ledger_transfer_fk FOREIGN KEY (ledger_transfer_id) REFERENCES openrails.ledger_transfers(id);

ALTER TABLE ONLY openrails.invoice_payments
    ADD CONSTRAINT invoice_payments_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.invoice_payments
    ADD CONSTRAINT invoice_payments_rail_merchant_account_fk FOREIGN KEY (rail_merchant_account_id) REFERENCES openrails.rail_merchant_accounts(id);

CREATE INDEX idx_invoice_payments_rail_merchant_account ON openrails.invoice_payments USING btree (rail_merchant_account_id) WHERE (rail_merchant_account_id IS NOT NULL);

CREATE INDEX ix_invoice_payments_invoice ON openrails.invoice_payments USING btree (merchant_id, invoice_id, created_at DESC);

CREATE UNIQUE INDEX uq_invoice_payments_ledger_transfer ON openrails.invoice_payments USING btree (merchant_id, ledger_transfer_id) WHERE (ledger_transfer_id IS NOT NULL);

ALTER TABLE openrails.invoice_payments ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.invoice_payments USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoice_payments TO openrails_app;

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

COMMENT ON TABLE openrails.invoker_spend_limits IS 'Per-invoker spend limits (#473/#517): the payer caps how much a delegated invoker/role can spend of the payer''s money. {scope, scope_key, windows[]} composed in one admit verdict over the payer balance. Payer-set only.';

COMMENT ON COLUMN openrails.invoker_spend_limits.scope_key IS 'Immutable scope discriminator: role uuid (scope=role), invoker string (scope=invoker), or tier key (scope=invoker_tier).';

ALTER TABLE ONLY openrails.invoker_spend_limits
    ADD CONSTRAINT invoker_spend_limits_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.invoker_spend_limits
    ADD CONSTRAINT invoker_spend_limits_uniq UNIQUE (merchant_id, customer_id, scope, scope_key);

ALTER TABLE ONLY openrails.invoker_spend_limits
    ADD CONSTRAINT invoker_spend_limits_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.invoker_spend_limits
    ADD CONSTRAINT invoker_spend_limits_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.invoker_spend_limits ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.invoker_spend_limits USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.invoker_spend_limits TO openrails_app;

CREATE TABLE openrails.merchant_configurations (
    merchant_id uuid NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    config_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.merchant_configurations FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.merchant_configurations IS 'One merchant-scoped JSON configuration row. Missing keys use service defaults.';

COMMENT ON COLUMN openrails.merchant_configurations.config IS 'JSONB merchant config. delegated_invoker_wasted_spend_windows is an array of {key, window_seconds, limit}; amount values use the request currency internal precision.';

ALTER TABLE ONLY openrails.merchant_configurations
    ADD CONSTRAINT merchant_configurations_pkey PRIMARY KEY (merchant_id);

ALTER TABLE ONLY openrails.merchant_configurations
    ADD CONSTRAINT merchant_configurations_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id);

ALTER TABLE openrails.merchant_configurations ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.merchant_configurations USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_configurations TO openrails_app;

CREATE TABLE openrails.merchant_deks (
    merchant_id uuid NOT NULL,
    wrapped_dek bytea NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

ALTER TABLE ONLY openrails.merchant_deks FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.merchant_deks IS 'Wrapped per-merchant Data Encryption Keys for envelope encryption-at-rest (issue #227). wrapped_dek = merchant DEK sealed with the master key (AES-256-GCM, nonce||ct||tag). Master key lives in config/env (self-hosted) or KMS (production), never in the DB. Merchant-owned and RLS protected.';

COMMENT ON COLUMN openrails.merchant_deks.wrapped_dek IS 'AES-256-GCM(master_key, merchant_dek): nonce(12) || ciphertext(32) || tag(16).';

ALTER TABLE ONLY openrails.merchant_deks
    ADD CONSTRAINT pk_merchant_deks PRIMARY KEY (merchant_id);

ALTER TABLE ONLY openrails.merchant_deks
    ADD CONSTRAINT merchant_deks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.merchant_deks ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.merchant_deks USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_deks TO openrails_app;

CREATE TABLE openrails.merchant_exports (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    status text DEFAULT 'completed'::text NOT NULL,
    row_counts jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT merchant_exports_status_check CHECK ((status = 'completed'::text))
);

ALTER TABLE ONLY openrails.merchant_exports FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.merchant_exports IS 'Merchant logical-export bookkeeping (issue #225). Merchant deletion is gated on a completed export row (export-before-delete). Merchant-owned and RLS protected.';

ALTER TABLE ONLY openrails.merchant_exports
    ADD CONSTRAINT merchant_exports_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.merchant_exports
    ADD CONSTRAINT merchant_exports_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_merchant_exports_merchant ON openrails.merchant_exports USING btree (merchant_id, created_at DESC);

ALTER TABLE openrails.merchant_exports ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.merchant_exports USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_exports TO openrails_app;

CREATE TABLE openrails.merchant_notifications (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    severity text DEFAULT 'warning'::text NOT NULL,
    title text NOT NULL,
    body text DEFAULT ''::text NOT NULL,
    link text DEFAULT ''::text NOT NULL,
    rule_id uuid,
    data jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    read_at timestamp with time zone
);

ALTER TABLE ONLY openrails.merchant_notifications FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.merchant_notifications IS '#736 MERCHANT-operator-facing in_app alert store (console bell). rule_id references the source alert_rules row informationally (no FK: notifications outlive rule deletion).';

ALTER TABLE ONLY openrails.merchant_notifications
    ADD CONSTRAINT merchant_notifications_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.merchant_notifications
    ADD CONSTRAINT merchant_notifications_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX merchant_notifications_bell_idx ON openrails.merchant_notifications USING btree (merchant_id, read_at, created_at DESC);

ALTER TABLE openrails.merchant_notifications ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.merchant_notifications USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_notifications TO openrails_app;

CREATE TABLE openrails.merchant_secrets (
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    value text NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

ALTER TABLE ONLY openrails.merchant_secrets FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.merchant_secrets IS 'DB-backed per-merchant secret store (issue #225). Namespaced by (merchant_id, name). The Vault-backed store keeps the same addressing but holds values in Vault. Merchant-owned and RLS protected.';

ALTER TABLE ONLY openrails.merchant_secrets
    ADD CONSTRAINT pk_merchant_secrets PRIMARY KEY (merchant_id, name);

ALTER TABLE ONLY openrails.merchant_secrets
    ADD CONSTRAINT merchant_secrets_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.merchant_secrets ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.merchant_secrets USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_secrets TO openrails_app;

CREATE TABLE openrails.merchant_webhooks (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    url text NOT NULL,
    format text DEFAULT 'generic'::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT merchant_webhooks_format_check CHECK ((format = ANY (ARRAY['generic'::text, 'discord'::text, 'slack'::text])))
);

ALTER TABLE ONLY openrails.merchant_webhooks FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.merchant_webhooks IS '#736 operator-configured OUTBOUND alert sinks. format shapes the POST body: generic=our alert JSON, discord={content}, slack={text}. NOT the inbound provider-webhook ingestion surface.';

ALTER TABLE ONLY openrails.merchant_webhooks
    ADD CONSTRAINT merchant_webhooks_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.merchant_webhooks
    ADD CONSTRAINT merchant_webhooks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX merchant_webhooks_merchant_idx ON openrails.merchant_webhooks USING btree (merchant_id);

ALTER TABLE openrails.merchant_webhooks ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.merchant_webhooks USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_webhooks TO openrails_app;

CREATE TABLE openrails.metered_rating_watermarks (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    source text NOT NULL,
    period_from timestamp with time zone NOT NULL,
    rated_through timestamp with time zone NOT NULL,
    accrued_amount bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT metered_rating_watermarks_accrued_nonneg CHECK ((accrued_amount >= 0))
);

ALTER TABLE ONLY openrails.metered_rating_watermarks FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.metered_rating_watermarks IS '#672 per-period metered-rating watermark: cumulative accrued amount + rated-through cutoff per (payer, currency, meter source, period start), so overlapping invoice closes bill each unit of usage exactly once.';

COMMENT ON COLUMN openrails.metered_rating_watermarks.source IS 'Meter accrual source key (metered:<meter>[:rate_card:<id>][:dim:<value>]).';

COMMENT ON COLUMN openrails.metered_rating_watermarks.accrued_amount IS 'Micros already accrued for [period_from, rated_through); the sweep accrues only the delta above this.';

ALTER TABLE ONLY openrails.metered_rating_watermarks
    ADD CONSTRAINT metered_rating_watermarks_pkey PRIMARY KEY (merchant_id, customer_id, currency, source, period_from);

ALTER TABLE ONLY openrails.metered_rating_watermarks
    ADD CONSTRAINT metered_rating_watermarks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.metered_rating_watermarks ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.metered_rating_watermarks USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.metered_rating_watermarks TO openrails_app;

CREATE TABLE openrails.money_settings (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    billing_mode text DEFAULT 'prepaid'::text NOT NULL,
    low_balance_threshold bigint,
    auto_topup_enabled boolean DEFAULT false NOT NULL,
    auto_topup_amount bigint,
    auto_topup_payment_method_id uuid,
    default_credit_expiry_hours integer,
    last_alert_at timestamp with time zone,
    last_topup_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    tier text,
    tier_source text DEFAULT 'auto'::text NOT NULL,
    currency text NOT NULL,
    credit_limit_amount bigint DEFAULT 0 NOT NULL,
    CONSTRAINT money_settings_billing_mode_chk CHECK ((billing_mode = ANY (ARRAY['prepaid'::text, 'arrears'::text]))),
    CONSTRAINT money_settings_credit_limit_amount_nonneg_chk CHECK ((credit_limit_amount >= 0)),
    CONSTRAINT money_settings_tier_source_chk CHECK ((tier_source = ANY (ARRAY['auto'::text, 'admin'::text])))
);

ALTER TABLE ONLY openrails.money_settings FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.money_settings IS 'Per-(merchant, customer, currency) spend policy and money-in config. Amount values use the row currency internal precision. Admission reads billing_mode + credit_limit_amount + the ledger balance; per-invoker caps live in payer/invoker_spend_limits; arrears owed exposure is derived from open invoices.';

COMMENT ON COLUMN openrails.money_settings.low_balance_threshold IS 'Optional low-balance trigger in the row currency internal precision.';

COMMENT ON COLUMN openrails.money_settings.default_credit_expiry_hours IS 'per-account default credit-grant expiry in HOURS; NULL = no default.';

COMMENT ON COLUMN openrails.money_settings.tier_source IS 'auto = tier maintained by tier_schedule auto-graduation; admin = explicit override that auto-graduation must not overwrite.';

COMMENT ON COLUMN openrails.money_settings.currency IS 'System currency code (USD/EUR/JPY); the Go registry is the authority. Stablecoins and crypto tokens are payment assets, not account currencies.';

COMMENT ON COLUMN openrails.money_settings.credit_limit_amount IS 'Admin-set arrears credit line in the row currency internal precision. 0 = no arrears capacity; prepaid balance may still be spent.';

ALTER TABLE ONLY openrails.money_settings
    ADD CONSTRAINT money_settings_pkey PRIMARY KEY (merchant_id, customer_id, currency);

ALTER TABLE ONLY openrails.money_settings
    ADD CONSTRAINT money_settings_auto_topup_payment_method_fk FOREIGN KEY (auto_topup_payment_method_id) REFERENCES openrails.payment_methods(id) ON DELETE SET NULL;

ALTER TABLE ONLY openrails.money_settings
    ADD CONSTRAINT money_settings_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.money_settings
    ADD CONSTRAINT money_settings_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.money_settings ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.money_settings USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.money_settings TO openrails_app;

CREATE TABLE openrails.notification_queue (
    id uuid DEFAULT uuidv7() NOT NULL,
    event_type text NOT NULL,
    data jsonb NOT NULL,
    seen boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    emailed_at timestamp with time zone
);

ALTER TABLE ONLY openrails.notification_queue FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.notification_queue IS 'Queue for user notifications related to billing and subscriptions';

COMMENT ON COLUMN openrails.notification_queue.emailed_at IS '#789: when the notification email was sent; NULL = undelivered (the notification_email_sweep retries).';

ALTER TABLE ONLY openrails.notification_queue
    ADD CONSTRAINT notification_queue_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.notification_queue
    ADD CONSTRAINT notification_queue_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.notification_queue
    ADD CONSTRAINT notification_queue_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_notification_queue_created_at ON openrails.notification_queue USING btree (created_at);

CREATE INDEX idx_notification_queue_customer ON openrails.notification_queue USING btree (customer_id) WHERE (customer_id IS NOT NULL);

CREATE INDEX idx_notification_queue_event_type ON openrails.notification_queue USING btree (event_type);

CREATE INDEX idx_notification_queue_merchant_id ON openrails.notification_queue USING btree (merchant_id);

CREATE INDEX idx_notification_queue_seen ON openrails.notification_queue USING btree (seen);

CREATE INDEX idx_notification_queue_undelivered ON openrails.notification_queue USING btree (merchant_id, created_at) WHERE (emailed_at IS NULL);

ALTER TABLE openrails.notification_queue ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.notification_queue USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.notification_queue TO openrails_app;

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

COMMENT ON TABLE openrails.payer_spend_limits IS 'Per-tier payer spend limit (#477/#517): the platform caps the payer''s spend, keyed by trust-tier. customer_id NULL is the merchant-wide default; non-NULL is a per-customer override.';

COMMENT ON COLUMN openrails.payer_spend_limits.customer_id IS 'NULL = merchant-wide default tier limit (#477); non-NULL = per-customer override taking precedence for that customer.';

COMMENT ON COLUMN openrails.payer_spend_limits.policy IS 'JSONB tier money policy: budget_windows and bad_spend_windows. Money values use the request currency internal precision.';

ALTER TABLE ONLY openrails.payer_spend_limits
    ADD CONSTRAINT payer_spend_limits_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.payer_spend_limits
    ADD CONSTRAINT payer_spend_limits_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.payer_spend_limits
    ADD CONSTRAINT payer_spend_limits_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX uq_payer_spend_limits_customer ON openrails.payer_spend_limits USING btree (merchant_id, customer_id, tier) WHERE (customer_id IS NOT NULL);

CREATE UNIQUE INDEX uq_payer_spend_limits_merchant_default ON openrails.payer_spend_limits USING btree (merchant_id, tier) WHERE (customer_id IS NULL);

ALTER TABLE openrails.payer_spend_limits ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.payer_spend_limits USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payer_spend_limits TO openrails_app;

CREATE TABLE openrails.price_key_movements (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    price_id uuid NOT NULL,
    effective_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

ALTER TABLE ONLY openrails.price_key_movements FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.price_key_movements IS '#774: append-only log of when a price key''s current pointer moved to which price row. History, not row identity — a row can appear more than once (reactivation).';

ALTER TABLE ONLY openrails.price_key_movements
    ADD CONSTRAINT price_key_movements_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.price_key_movements
    ADD CONSTRAINT price_key_movements_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.price_key_movements
    ADD CONSTRAINT price_key_movements_price_fk FOREIGN KEY (price_id) REFERENCES openrails.prices(id) ON DELETE RESTRICT;

CREATE INDEX idx_price_key_movements_key ON openrails.price_key_movements USING btree (merchant_id, key, effective_at DESC);

CREATE INDEX idx_price_key_movements_price ON openrails.price_key_movements USING btree (price_id);

ALTER TABLE openrails.price_key_movements ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.price_key_movements USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.price_key_movements TO openrails_app;

CREATE TABLE openrails.product_includes (
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    included_product_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT product_includes_not_self CHECK ((product_id <> included_product_id))
);

ALTER TABLE ONLY openrails.product_includes FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.product_includes IS '#611 catalog bundle includes: parent product grants/owns included catalog products when materialized.';

ALTER TABLE ONLY openrails.product_includes
    ADD CONSTRAINT product_includes_pkey PRIMARY KEY (merchant_id, product_id, included_product_id);

ALTER TABLE ONLY openrails.product_includes
    ADD CONSTRAINT product_includes_included_product_fk FOREIGN KEY (included_product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;

ALTER TABLE ONLY openrails.product_includes
    ADD CONSTRAINT product_includes_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.product_includes
    ADD CONSTRAINT product_includes_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;

CREATE INDEX idx_product_includes_included_product ON openrails.product_includes USING btree (merchant_id, included_product_id);

ALTER TABLE openrails.product_includes ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.product_includes USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.product_includes TO openrails_app;

CREATE TABLE openrails.product_usage_limit_bindings (
    id uuid NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    usage_limit_key text NOT NULL,
    measure text NOT NULL,
    windows jsonb DEFAULT '[]'::jsonb NOT NULL,
    source_type text NOT NULL,
    source_id uuid,
    product_key text,
    grant_id uuid,
    starts_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    ends_at timestamp with time zone,
    revoked_at timestamp with time zone,
    policy_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT product_usage_limit_bindings_key_nonempty CHECK ((btrim(usage_limit_key) <> ''::text)),
    CONSTRAINT product_usage_limit_bindings_measure_nonempty CHECK ((btrim(measure) <> ''::text)),
    CONSTRAINT product_usage_limit_bindings_time_check CHECK (((ends_at IS NULL) OR (ends_at > starts_at))),
    CONSTRAINT product_usage_limit_bindings_windows_array CHECK ((jsonb_typeof(windows) = 'array'::text))
);

ALTER TABLE ONLY openrails.product_usage_limit_bindings FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.product_usage_limit_bindings IS '#594 materialized product-derived usage-limit bindings. Loaded into admission policy; live counters stay in Redis.';

COMMENT ON COLUMN openrails.product_usage_limit_bindings.product_key IS 'Catalog product key/slug whose current benefits were materialized at grant time.';

ALTER TABLE ONLY openrails.product_usage_limit_bindings
    ADD CONSTRAINT product_usage_limit_bindings_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.product_usage_limit_bindings
    ADD CONSTRAINT product_usage_limit_bindings_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.product_usage_limit_bindings
    ADD CONSTRAINT product_usage_limit_bindings_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_product_usage_limit_bindings_active ON openrails.product_usage_limit_bindings USING btree (merchant_id, customer_id, measure) WHERE (revoked_at IS NULL);

ALTER TABLE openrails.product_usage_limit_bindings ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.product_usage_limit_bindings USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.product_usage_limit_bindings TO openrails_app;

CREATE TABLE openrails.product_usage_limits (
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    usage_limit_key text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT product_usage_limits_key_nonempty CHECK ((btrim(usage_limit_key) <> ''::text))
);

ALTER TABLE ONLY openrails.product_usage_limits FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.product_usage_limits IS '#617 catalog product usage-limit memberships. Grants materialize these into customer product_usage_limit_bindings.';

ALTER TABLE ONLY openrails.product_usage_limits
    ADD CONSTRAINT product_usage_limits_pkey PRIMARY KEY (merchant_id, product_id, usage_limit_key);

ALTER TABLE ONLY openrails.product_usage_limits
    ADD CONSTRAINT product_usage_limits_limit_fk FOREIGN KEY (merchant_id, usage_limit_key) REFERENCES openrails.catalog_usage_limits(merchant_id, key) ON DELETE CASCADE;

ALTER TABLE ONLY openrails.product_usage_limits
    ADD CONSTRAINT product_usage_limits_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.product_usage_limits
    ADD CONSTRAINT product_usage_limits_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;

CREATE INDEX idx_product_usage_limits_key ON openrails.product_usage_limits USING btree (merchant_id, usage_limit_key);

ALTER TABLE openrails.product_usage_limits ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.product_usage_limits USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.product_usage_limits TO openrails_app;

CREATE TABLE openrails.rail_customer_accounts (
    id uuid DEFAULT uuidv7() NOT NULL,
    rail text NOT NULL,
    account_id text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL
);

ALTER TABLE ONLY openrails.rail_customer_accounts FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.rail_customer_accounts IS 'customer <-> rail customer-id mapping. Keyed per (merchant, customer, rail); rail_merchant_account_id provenance was dropped (#704) — no writer ever set it.';

ALTER TABLE ONLY openrails.rail_customer_accounts
    ADD CONSTRAINT rail_customer_accounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.rail_customer_accounts
    ADD CONSTRAINT rail_customer_accounts_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.rail_customer_accounts
    ADD CONSTRAINT rail_customer_accounts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_rail_customer_accounts_customer ON openrails.rail_customer_accounts USING btree (customer_id) WHERE (customer_id IS NOT NULL);

CREATE INDEX idx_rail_customer_accounts_merchant_id ON openrails.rail_customer_accounts USING btree (merchant_id);

CREATE UNIQUE INDEX uq_rail_customer_accounts_customer_rail ON openrails.rail_customer_accounts USING btree (merchant_id, customer_id, rail);

CREATE UNIQUE INDEX uq_rail_customer_accounts_merchant_rail_customer ON openrails.rail_customer_accounts USING btree (merchant_id, rail, account_id);

ALTER TABLE openrails.rail_customer_accounts ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.rail_customer_accounts USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.rail_customer_accounts TO openrails_app;

CREATE TABLE openrails.rail_intents (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
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
    actor text,
    last_failure_reason text,
    expires_at timestamp with time zone,
    result_evidence jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    executed_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    rail_merchant_account_id uuid,
    CONSTRAINT chk_rail_intents_executed CHECK (((status <> 'succeeded'::text) OR (executed_at IS NOT NULL))),
    CONSTRAINT chk_rail_intents_origin CHECK ((origin = ANY (ARRAY['user'::text, 'admin'::text, 'system'::text]))),
    CONSTRAINT chk_rail_intents_status CHECK ((status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'succeeded'::text, 'unknown_needs_verify'::text, 'failed_retryable'::text, 'failed_terminal'::text, 'superseded'::text, 'expired'::text])))
);

ALTER TABLE ONLY openrails.rail_intents FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.rail_intents IS 'Durable, effectively-once outbox for outbound provider mutations (#358). One row per logical intent (unique per tenant on idempotency_key); the executor worker drains whatever is currently executable, the verifier resolves ambiguous outcomes via provider reads.';

COMMENT ON COLUMN openrails.rail_intents.rail IS 'Rail the mutation targets (e.g. ''nmi'', ''stripe'').';

COMMENT ON COLUMN openrails.rail_intents.intent_type IS 'Registry key selecting the per-type semantics (executor, verifier, relevance, backoff): nmi_delete_subscription, and in later phases manual_rebill, refund, plan_archive, ...';

COMMENT ON COLUMN openrails.rail_intents.idempotency_key IS 'Deterministic identity of the logical intent within the tenant. Re-enqueues conflict here: a pending intent is refreshed, a superseded/expired one revived (relevance returned), anything else untouched — effectively-once per logical intent.';

COMMENT ON COLUMN openrails.rail_intents.claimed_until IS 'Single-executor lease (SKIP LOCKED claim). An in_flight row whose lease elapsed was orphaned by a crashed executor and becomes claimable again; per-type execute semantics (verify-then-execute, verifier-before-retry) make the reclaim safe.';

COMMENT ON COLUMN openrails.rail_intents.origin IS 'Who wanted this mutation: user/admin-origin intents execute under mode=limited (reactive completion), system-origin intents require mode=full. Nothing executes under mode=readonly.';

COMMENT ON COLUMN openrails.rail_intents.actor IS 'Authenticated principal id (admin user id or self-service customer id) that produced a user/admin-origin intent. NULL for system-origin. Powers the #732 anti-credential-compromise rate ceiling (per-actor + global rolling-hour count of destructive ops).';

COMMENT ON COLUMN openrails.rail_intents.last_failure_reason IS 'Why the most recent attempt did not succeed (mode parked, kill switch, provider down, declined...). Recorded on the intent, never surfaced as an error.';

COMMENT ON COLUMN openrails.rail_intents.expires_at IS 'End of the relevance window: past this instant the intent expires with a finding instead of firing stale (NULL = relevance governed solely by the type''s relevance check).';

COMMENT ON COLUMN openrails.rail_intents.result_evidence IS 'How the terminal status was established (e.g. {"verified_absent": true} for a delete confirmed by a provider read).';

COMMENT ON COLUMN openrails.rail_intents.rail_merchant_account_id IS 'Provider account row the outbound intent was enqueued against. Mismatch with current credentials parks/defers execution.';

ALTER TABLE ONLY openrails.rail_intents
    ADD CONSTRAINT rail_intents_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.rail_intents
    ADD CONSTRAINT rail_intents_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.rail_intents
    ADD CONSTRAINT rail_intents_rail_merchant_account_fk FOREIGN KEY (rail_merchant_account_id) REFERENCES openrails.rail_merchant_accounts(id);

CREATE INDEX idx_rail_intents_actor_created ON openrails.rail_intents USING btree (actor, created_at) WHERE (actor IS NOT NULL);

CREATE INDEX idx_rail_intents_created ON openrails.rail_intents USING btree (created_at);

CREATE INDEX idx_rail_intents_due ON openrails.rail_intents USING btree (next_attempt_at) WHERE (status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'failed_retryable'::text, 'unknown_needs_verify'::text]));

CREATE INDEX idx_rail_intents_merchant_id ON openrails.rail_intents USING btree (merchant_id);

CREATE INDEX idx_rail_intents_rail_merchant_account ON openrails.rail_intents USING btree (rail_merchant_account_id) WHERE (rail_merchant_account_id IS NOT NULL);

CREATE INDEX idx_rail_intents_subscription ON openrails.rail_intents USING btree (subscription_id) WHERE (subscription_id IS NOT NULL);

CREATE UNIQUE INDEX uq_rail_intents_merchant_idempotency_key ON openrails.rail_intents USING btree (merchant_id, idempotency_key);

ALTER TABLE openrails.rail_intents ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.rail_intents USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.rail_intents TO openrails_app;

CREATE TABLE openrails.rail_mutation_logs (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    rail_merchant_account_id uuid,
    rail_intent_id uuid,
    intent_type text,
    idempotency_key text,
    attempt integer DEFAULT 0 NOT NULL,
    phase text NOT NULL,
    reason text,
    evidence jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT rail_mutation_logs_phase_check CHECK ((phase = ANY (ARRAY['attempting'::text, 'succeeded'::text, 'failed'::text, 'unknown'::text, 'parked'::text])))
);

ALTER TABLE ONLY openrails.rail_mutation_logs FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.rail_mutation_logs IS 'Append-only operator history for external provider mutations executed from provider intents/convergence (#533).';

COMMENT ON COLUMN openrails.rail_mutation_logs.phase IS 'Provider mutation lifecycle phase: attempting before the remote call, then succeeded/failed/unknown/parked after the handler classifies the result.';

COMMENT ON COLUMN openrails.rail_mutation_logs.evidence IS 'Scrubbed structured metadata only. Never store API keys, authorization headers, card data, private keys, or unsanitized provider bodies.';

ALTER TABLE ONLY openrails.rail_mutation_logs
    ADD CONSTRAINT rail_mutation_logs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.rail_mutation_logs
    ADD CONSTRAINT rail_mutation_logs_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

ALTER TABLE ONLY openrails.rail_mutation_logs
    ADD CONSTRAINT rail_mutation_logs_rail_intent_fk FOREIGN KEY (rail_intent_id) REFERENCES openrails.rail_intents(id) ON DELETE SET NULL;

ALTER TABLE ONLY openrails.rail_mutation_logs
    ADD CONSTRAINT rail_mutation_logs_rail_merchant_account_fk FOREIGN KEY (rail_merchant_account_id) REFERENCES openrails.rail_merchant_accounts(id) ON DELETE SET NULL;

CREATE INDEX idx_rail_mutation_logs_merchant_created ON openrails.rail_mutation_logs USING btree (merchant_id, created_at DESC);

CREATE INDEX idx_rail_mutation_logs_rail_intent ON openrails.rail_mutation_logs USING btree (rail_intent_id) WHERE (rail_intent_id IS NOT NULL);

CREATE INDEX idx_rail_mutation_logs_rail_merchant_account ON openrails.rail_mutation_logs USING btree (rail_merchant_account_id) WHERE (rail_merchant_account_id IS NOT NULL);

CREATE INDEX idx_rail_mutation_logs_rail_phase ON openrails.rail_mutation_logs USING btree (rail, phase, created_at DESC);

ALTER TABLE openrails.rail_mutation_logs ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.rail_mutation_logs USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.rail_mutation_logs TO openrails_app;

CREATE TABLE openrails.rail_refresh_watermarks (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    rail_merchant_account_id uuid,
    rail_merchant_account_key uuid GENERATED ALWAYS AS (COALESCE(rail_merchant_account_id, '00000000-0000-0000-0000-000000000000'::uuid)) STORED,
    event_domain text NOT NULL,
    watermark_at timestamp with time zone NOT NULL,
    last_attempted_at timestamp with time zone,
    last_succeeded_at timestamp with time zone,
    last_error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT rail_refresh_watermarks_account_nonzero CHECK (((rail_merchant_account_id IS NULL) OR (rail_merchant_account_id <> '00000000-0000-0000-0000-000000000000'::uuid))),
    CONSTRAINT rail_refresh_watermarks_event_domain_check CHECK ((event_domain = ANY (ARRAY['events'::text]))),
    CONSTRAINT rail_refresh_watermarks_rail_check CHECK ((rail = ANY (ARRAY['nmi'::text, 'ccbill'::text, 'stripe'::text, 'solana'::text])))
);

ALTER TABLE ONLY openrails.rail_refresh_watermarks FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.rail_refresh_watermarks IS 'Durable Provider Refresh watermarks. A failed or partial provider read records last_error but never advances watermark_at.';

COMMENT ON COLUMN openrails.rail_refresh_watermarks.rail_merchant_account_id IS 'Current provider account row when resolvable; NULL is the compatibility/global lane for providers without a bound account identity.';

COMMENT ON COLUMN openrails.rail_refresh_watermarks.event_domain IS 'Refresh domain. events currently covers provider transaction/subscription event windows.';

COMMENT ON COLUMN openrails.rail_refresh_watermarks.watermark_at IS 'Exclusive lower bound for the next successful bounded provider event refresh window.';

ALTER TABLE ONLY openrails.rail_refresh_watermarks
    ADD CONSTRAINT rail_refresh_watermarks_identity_key UNIQUE (merchant_id, rail, rail_merchant_account_key, event_domain);

ALTER TABLE ONLY openrails.rail_refresh_watermarks
    ADD CONSTRAINT rail_refresh_watermarks_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.rail_refresh_watermarks
    ADD CONSTRAINT rail_refresh_watermarks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

ALTER TABLE ONLY openrails.rail_refresh_watermarks
    ADD CONSTRAINT rail_refresh_watermarks_rail_merchant_account_fk FOREIGN KEY (rail_merchant_account_id) REFERENCES openrails.rail_merchant_accounts(id) ON DELETE SET NULL;

CREATE INDEX idx_rail_refresh_watermarks_account ON openrails.rail_refresh_watermarks USING btree (rail_merchant_account_id) WHERE (rail_merchant_account_id IS NOT NULL);

CREATE INDEX idx_rail_refresh_watermarks_rail ON openrails.rail_refresh_watermarks USING btree (rail, event_domain, watermark_at);

ALTER TABLE openrails.rail_refresh_watermarks ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.rail_refresh_watermarks USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.rail_refresh_watermarks TO openrails_app;

CREATE TABLE openrails.reconciliation_runs (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    mode text NOT NULL,
    rails text[] NOT NULL,
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

COMMENT ON TABLE openrails.reconciliation_runs IS 'One row per manual reconcile run (#107): advisory diffs or enforce convergence against the payment rails. Summary jsonb carries per-rail counts and the dunning-forensics report.';

ALTER TABLE ONLY openrails.reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_reconciliation_runs_merchant_id ON openrails.reconciliation_runs USING btree (merchant_id);

CREATE INDEX idx_reconciliation_runs_started_at ON openrails.reconciliation_runs USING btree (started_at DESC);

ALTER TABLE openrails.reconciliation_runs ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.reconciliation_runs USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.reconciliation_runs TO openrails_app;

CREATE TABLE openrails.reconciliation_findings (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    finding_type text NOT NULL,
    subject_key text NOT NULL,
    severity text NOT NULL,
    status text DEFAULT 'open'::text NOT NULL,
    recommended_action text,
    first_seen_run uuid,
    last_seen_run uuid,
    last_seen_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    resolved_at timestamp with time zone,
    resolution text,
    operator_notes text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    evidence jsonb,
    resolved_by text,
    notified_at timestamp with time zone,
    notified_severity text,
    CONSTRAINT chk_reconciliation_findings_resolution CHECK (((resolution IS NULL) OR (resolution = ANY (ARRAY['auto_vanished'::text, 'enforced'::text, 'admin_fixed'::text, 'ignored'::text])))),
    CONSTRAINT chk_reconciliation_findings_resolved_fields CHECK ((((status = ANY (ARRAY['auto_fixed'::text, 'fixed'::text, 'ignored'::text])) AND (resolved_at IS NOT NULL) AND (resolution IS NOT NULL)) OR ((status = ANY (ARRAY['reconcile_required'::text, 'requires_review'::text])) AND (resolved_at IS NULL) AND (resolution IS NULL)))),
    CONSTRAINT chk_reconciliation_findings_severity CHECK ((severity = ANY (ARRAY['critical'::text, 'high'::text, 'medium'::text, 'low'::text]))),
    CONSTRAINT chk_reconciliation_findings_status CHECK ((status = ANY (ARRAY['auto_fixed'::text, 'reconcile_required'::text, 'requires_review'::text, 'fixed'::text, 'ignored'::text]))),
    CONSTRAINT chk_reconciliation_findings_type CHECK ((finding_type ~ '^(pull|derive|life|consistency|notify)\.[a-z0-9_]+(\.[a-z0-9_]+)?$'::text))
);

ALTER TABLE ONLY openrails.reconciliation_findings FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.reconciliation_findings IS 'Durable reconciliation findings ledger. Stable identity per (merchant, finding_type, subject_key); provider/account context lives in evidence for pull.* findings. Statuses: reconcile_required, requires_review, auto_fixed, fixed, ignored (#573).';

COMMENT ON COLUMN openrails.reconciliation_findings.subject_key IS 'Stable identity of the drifted subject within (provider, finding_type): rail subscription id, transaction id, local subscription/payment-method uuid, or tenant_subject uuid depending on the check.';

COMMENT ON COLUMN openrails.reconciliation_findings.first_seen_run IS 'Reconciliation run that first observed this finding; NULL when raised outside a run (e.g. the intents volume breaker).';

COMMENT ON COLUMN openrails.reconciliation_findings.operator_notes IS 'Operator-entered notes attached when a finding is fixed or ignored manually.';

COMMENT ON COLUMN openrails.reconciliation_findings.evidence IS 'Machine-readable finding evidence. Optional nested keys: provider, local, remote, intent, resolution.';

COMMENT ON COLUMN openrails.reconciliation_findings.resolved_by IS 'Authenticated admin identity stamped on manual resolution (approve/ignore via the findings queue, #692); NULL for automatic resolutions.';

COMMENT ON COLUMN openrails.reconciliation_findings.notified_at IS '#787: last time this OPEN finding pushed an operator notification; NULL = not yet notified this open episode. Cleared to NULL on every resolution so a reopened finding notifies again.';

COMMENT ON COLUMN openrails.reconciliation_findings.notified_severity IS '#787: severity at last notification; a further increase while still open re-fires, re-observation at the same/lower severity does not.';

ALTER TABLE ONLY openrails.reconciliation_findings
    ADD CONSTRAINT reconciliation_findings_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.reconciliation_findings
    ADD CONSTRAINT reconciliation_findings_first_seen_run_fk FOREIGN KEY (first_seen_run) REFERENCES openrails.reconciliation_runs(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.reconciliation_findings
    ADD CONSTRAINT reconciliation_findings_last_seen_run_fk FOREIGN KEY (last_seen_run) REFERENCES openrails.reconciliation_runs(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.reconciliation_findings
    ADD CONSTRAINT reconciliation_findings_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_reconciliation_findings_actionable ON openrails.reconciliation_findings USING btree (finding_type) WHERE (status = ANY (ARRAY['reconcile_required'::text, 'requires_review'::text]));

CREATE INDEX idx_reconciliation_findings_low_severity_pending_digest ON openrails.reconciliation_findings USING btree (merchant_id) WHERE ((status = 'requires_review'::text) AND (severity = 'low'::text) AND (notified_at IS NULL));

CREATE INDEX idx_reconciliation_findings_merchant_id ON openrails.reconciliation_findings USING btree (merchant_id);

CREATE INDEX idx_reconciliation_findings_requires_review ON openrails.reconciliation_findings USING btree (last_seen_at DESC) WHERE (status = 'requires_review'::text);

CREATE UNIQUE INDEX uq_reconciliation_findings_identity ON openrails.reconciliation_findings USING btree (merchant_id, finding_type, subject_key);

ALTER TABLE openrails.reconciliation_findings ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.reconciliation_findings USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.reconciliation_findings TO openrails_app;

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

COMMENT ON TABLE openrails.reconciliation_state IS '#511 per-(merchant, source_domain) reconciliation watermark. fully_reconciled gates the confirmed-absence rule: a destructive EXCESS repair is HELD until its source domain (subscriptions|payments|grants) is proven fully reconciled.';

ALTER TABLE ONLY openrails.reconciliation_state
    ADD CONSTRAINT reconciliation_state_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.reconciliation_state
    ADD CONSTRAINT reconciliation_state_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX uq_reconciliation_state_identity ON openrails.reconciliation_state USING btree (merchant_id, source_domain);

ALTER TABLE openrails.reconciliation_state ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.reconciliation_state USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.reconciliation_state TO openrails_app;

CREATE TABLE openrails.reprice_batches (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    price_key text,
    to_price_id uuid NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    subscriptions_matched integer DEFAULT 0 NOT NULL,
    subscriptions_scheduled integer DEFAULT 0 NOT NULL,
    subscriptions_skipped integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

ALTER TABLE ONLY openrails.reprice_batches FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.reprice_batches IS '#773: header row for one bulk reprice operation (reprice_all_prior_versions or a single ad-hoc reprice); subscription_reprices rows carry reprice_batch_id back to it for per-subscription progress.';

ALTER TABLE ONLY openrails.reprice_batches
    ADD CONSTRAINT reprice_batches_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.reprice_batches
    ADD CONSTRAINT reprice_batches_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.reprice_batches
    ADD CONSTRAINT reprice_batches_to_price_fk FOREIGN KEY (to_price_id) REFERENCES openrails.prices(id) ON DELETE RESTRICT;

CREATE INDEX idx_reprice_batches_merchant ON openrails.reprice_batches USING btree (merchant_id, created_at DESC);

ALTER TABLE openrails.reprice_batches ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.reprice_batches USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.reprice_batches TO openrails_app;

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

ALTER TABLE ONLY openrails.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_subscription_pda_key UNIQUE (subscription_pda);

ALTER TABLE ONLY openrails.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_subscription_id_fkey FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE CASCADE;

CREATE INDEX idx_solana_subscriptions_due ON openrails.solana_subscriptions USING btree (merchant_id, next_pull_at) WHERE (status = 'active'::text);

CREATE INDEX idx_solana_subscriptions_subscription_id ON openrails.solana_subscriptions USING btree (subscription_id);

ALTER TABLE openrails.solana_subscriptions ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.solana_subscriptions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.solana_subscriptions TO openrails_app;

CREATE TABLE openrails.subscription_reprices (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    from_price_id uuid NOT NULL,
    to_price_id uuid NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    status text DEFAULT 'scheduled'::text NOT NULL,
    reprice_batch_id uuid,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    applied_at timestamp with time zone,
    canceled_at timestamp with time zone,
    acknowledged_short_notice boolean DEFAULT false NOT NULL,
    CONSTRAINT subscription_reprices_applied_has_timestamp CHECK (((status <> 'applied'::text) OR (applied_at IS NOT NULL))),
    CONSTRAINT subscription_reprices_canceled_has_timestamp CHECK (((status <> 'canceled'::text) OR (canceled_at IS NOT NULL))),
    CONSTRAINT subscription_reprices_status_chk CHECK ((status = ANY (ARRAY['scheduled'::text, 'applied'::text, 'canceled'::text])))
);

ALTER TABLE ONLY openrails.subscription_reprices FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.subscription_reprices IS '#773: a scheduled, applied, or canceled price move for one subscription. Applied at the subscription''s first renewal on/after effective_at (v1: no proration/mid-cycle).';

COMMENT ON COLUMN openrails.subscription_reprices.acknowledged_short_notice IS '#781: true when this INCREASE reprice''s effective_at was inside the merchant''s configured notice window and was scheduled anyway via the explicit acknowledge_short_notice override on the request — the audit record for the support/emergency bypass path.';

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_batch_fk FOREIGN KEY (reprice_batch_id) REFERENCES openrails.reprice_batches(id) ON DELETE SET NULL;

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_from_price_fk FOREIGN KEY (from_price_id) REFERENCES openrails.prices(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_subscription_fk FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.subscription_reprices
    ADD CONSTRAINT subscription_reprices_to_price_fk FOREIGN KEY (to_price_id) REFERENCES openrails.prices(id) ON DELETE RESTRICT;

CREATE INDEX idx_subscription_reprices_batch ON openrails.subscription_reprices USING btree (reprice_batch_id) WHERE (reprice_batch_id IS NOT NULL);

CREATE INDEX idx_subscription_reprices_due ON openrails.subscription_reprices USING btree (effective_at) WHERE (status = 'scheduled'::text);

CREATE INDEX idx_subscription_reprices_merchant ON openrails.subscription_reprices USING btree (merchant_id, created_at DESC);

CREATE INDEX idx_subscription_reprices_subscription ON openrails.subscription_reprices USING btree (subscription_id);

CREATE UNIQUE INDEX uq_subscription_reprices_one_scheduled ON openrails.subscription_reprices USING btree (subscription_id) WHERE (status = 'scheduled'::text);

ALTER TABLE openrails.subscription_reprices ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.subscription_reprices USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.subscription_reprices TO openrails_app;

CREATE TABLE openrails.subscription_status_transitions (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    from_status openrails.subscription_status,
    to_status openrails.subscription_status NOT NULL,
    cancel_type text,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_sst_real_transition CHECK ((from_status IS DISTINCT FROM to_status))
);

ALTER TABLE ONLY openrails.subscription_status_transitions FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.subscription_status_transitions IS '#733 append-only subscription status audit, written by trg_subscriptions_status_transition in the SAME tx as the status change. from_status NULL = row creation. Not retroactive: history begins at go-live.';

COMMENT ON COLUMN openrails.subscription_status_transitions.cancel_type IS 'cancel_type on the subscription at transition time (meaningful for to_status=cancelled).';

ALTER TABLE ONLY openrails.subscription_status_transitions
    ADD CONSTRAINT subscription_status_transitions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.subscription_status_transitions
    ADD CONSTRAINT sst_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.subscription_status_transitions
    ADD CONSTRAINT sst_subscription_fk FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE CASCADE;

CREATE INDEX idx_sst_merchant_occurred ON openrails.subscription_status_transitions USING btree (merchant_id, occurred_at);

CREATE INDEX idx_sst_subscription ON openrails.subscription_status_transitions USING btree (subscription_id, occurred_at);

ALTER TABLE openrails.subscription_status_transitions ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.subscription_status_transitions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT ON TABLE openrails.subscription_status_transitions TO openrails_app;

CREATE TABLE openrails.tier_schedules (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid,
    currency text NOT NULL,
    rungs jsonb DEFAULT '[]'::jsonb NOT NULL,
    schedule_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.tier_schedules FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.tier_schedules IS 'Persisted tier ladder (#476): rungs declared once per merchant and currency, or as a per-customer/currency override. Platform-owned (subjects cannot edit). OpenRails auto-maintains money_settings.tier from same-currency cumulative paid spend unless tier_source=admin.';

COMMENT ON COLUMN openrails.tier_schedules.customer_id IS 'NULL = merchant-wide default schedule for this currency; non-NULL = per-customer override taking precedence for that customer/currency.';

COMMENT ON COLUMN openrails.tier_schedules.currency IS 'Currency whose cumulative paid amount is compared to this ladder.';

COMMENT ON COLUMN openrails.tier_schedules.rungs IS 'Ordered JSONB array of {tier, min_cumulative_paid_amount}; a payer''s tier = highest rung whose min_cumulative_paid_amount <= same-currency cumulative_paid.';

ALTER TABLE ONLY openrails.tier_schedules
    ADD CONSTRAINT tier_schedules_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.tier_schedules
    ADD CONSTRAINT tier_schedules_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.tier_schedules
    ADD CONSTRAINT tier_schedules_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX uq_tier_schedules_customer ON openrails.tier_schedules USING btree (merchant_id, customer_id, currency) WHERE (customer_id IS NOT NULL);

CREATE UNIQUE INDEX uq_tier_schedules_merchant_default ON openrails.tier_schedules USING btree (merchant_id, currency) WHERE (customer_id IS NULL);

ALTER TABLE openrails.tier_schedules ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.tier_schedules USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.tier_schedules TO openrails_app;

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
    ledger_transfer_id uuid,
    metadata jsonb,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT usage_events_amount_check CHECK ((amount >= 0))
);

ALTER TABLE ONLY openrails.usage_events FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.usage_events IS 'Append-only multi-dimensional metered usage (issue #289). Source of truth for usage reporting + #303 invoice line items. Host-priced (amount sent by the host); event + ledger debit commit in one tx. The hot admission path (#298) never reads this table.';

COMMENT ON COLUMN openrails.usage_events.invoker_id IS 'Caller-supplied principal string that fired this metered usage event. Opaque to OpenRails; attribution + grouping only, not a FK. Joins use source/source_id.';

COMMENT ON COLUMN openrails.usage_events.currency IS 'Native OpenRails currency code; amount uses this currency internal precision.';

COMMENT ON COLUMN openrails.usage_events.resource IS 'Caller-supplied free-form string for what was metered (tensorhub: endpoint slug; doujins: plan/item slug). Opaque to OpenRails; nullable, not a FK.';

ALTER TABLE ONLY openrails.usage_events
    ADD CONSTRAINT usage_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.usage_events
    ADD CONSTRAINT usage_events_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.usage_events
    ADD CONSTRAINT usage_events_ledger_transfer_fk FOREIGN KEY (ledger_transfer_id) REFERENCES openrails.ledger_transfers(id);

ALTER TABLE ONLY openrails.usage_events
    ADD CONSTRAINT usage_events_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_usage_events_customer_time ON openrails.usage_events USING btree (customer_id, occurred_at);

CREATE INDEX idx_usage_events_invoker ON openrails.usage_events USING btree (merchant_id, invoker_id, occurred_at DESC);

CREATE INDEX idx_usage_events_merchant_occurred ON openrails.usage_events USING btree (merchant_id, occurred_at);

CREATE INDEX ix_usage_events_payer_time ON openrails.usage_events USING btree (merchant_id, customer_id, occurred_at);

CREATE INDEX ix_usage_events_payer_type_time ON openrails.usage_events USING btree (merchant_id, customer_id, event_type, occurred_at);

CREATE UNIQUE INDEX uq_usage_events_idem ON openrails.usage_events USING btree (merchant_id, customer_id, currency, event_type, source, source_id);

ALTER TABLE openrails.usage_events ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.usage_events USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.usage_events TO openrails_app;

CREATE TABLE openrails.webhook_events (
    merchant_id uuid NOT NULL,
    op text NOT NULL,
    event_id text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    completed_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

ALTER TABLE ONLY openrails.webhook_events FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.webhook_events IS '#678 webhook dedup truth: one row per applied webhook event (merchant, op, event_id). Pending/lease state stays in Redis (coordination, not truth); a row here means effects are durably applied.';

COMMENT ON COLUMN openrails.webhook_events.op IS 'Dedup operation key, webhook.<rail>.<event_type> — matches the Redis key derivation.';

ALTER TABLE ONLY openrails.webhook_events
    ADD CONSTRAINT webhook_events_pkey PRIMARY KEY (merchant_id, op, event_id);

ALTER TABLE ONLY openrails.webhook_events
    ADD CONSTRAINT webhook_events_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_webhook_events_completed_at ON openrails.webhook_events USING btree (completed_at);

ALTER TABLE openrails.webhook_events ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.webhook_events USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE ON TABLE openrails.webhook_events TO openrails_app;

CREATE TABLE openrails.webhook_health (
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    last_accepted_at timestamp with time zone,
    accepted_count bigint DEFAULT 0 NOT NULL,
    last_rejected_at timestamp with time zone,
    rejected_count bigint DEFAULT 0 NOT NULL,
    last_drift_at timestamp with time zone,
    drift_count bigint DEFAULT 0 NOT NULL,
    last_pull_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT webhook_health_rail_nonempty CHECK ((btrim(rail) <> ''::text))
);

ALTER TABLE ONLY openrails.webhook_health FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.webhook_health IS '#786 per-(merchant, rail) inbound-webhook health: accepted/rejected/drift watermarks + counters. last_accepted_at is stamped only by signature-verified webhooks; last_pull_at is the provider-refresh pull watermark the drift gate uses.';

COMMENT ON COLUMN openrails.webhook_health.last_accepted_at IS 'last signature-VERIFIED webhook for this rail; silence age is measured from here (or created_at when nothing was ever accepted).';

COMMENT ON COLUMN openrails.webhook_health.drift_count IS 'pull-derived corrections applied while last_accepted_at predated the previous pull — changes a webhook should have announced.';

ALTER TABLE ONLY openrails.webhook_health
    ADD CONSTRAINT webhook_health_pkey PRIMARY KEY (merchant_id, rail);

ALTER TABLE ONLY openrails.webhook_health
    ADD CONSTRAINT webhook_health_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

ALTER TABLE openrails.webhook_health ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.webhook_health USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.webhook_health TO openrails_app;

CREATE TABLE openrails.webhook_health_daily (
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    day_at timestamp with time zone NOT NULL,
    accepted bigint DEFAULT 0 NOT NULL,
    rejected bigint DEFAULT 0 NOT NULL,
    drift bigint DEFAULT 0 NOT NULL,
    CONSTRAINT webhook_health_daily_rail_nonempty CHECK ((btrim(rail) <> ''::text))
);

ALTER TABLE ONLY openrails.webhook_health_daily FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.webhook_health_daily IS '#786 UTC-day webhook counter buckets backing the #733 webhook_rejects / webhook_drift_events windowed metrics.';

ALTER TABLE ONLY openrails.webhook_health_daily
    ADD CONSTRAINT webhook_health_daily_pkey PRIMARY KEY (merchant_id, rail, day_at);

ALTER TABLE ONLY openrails.webhook_health_daily
    ADD CONSTRAINT webhook_health_daily_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

ALTER TABLE openrails.webhook_health_daily ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.webhook_health_daily USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.webhook_health_daily TO openrails_app;

-- ---------------------------------------------------------------------------
-- Views
-- ---------------------------------------------------------------------------

CREATE VIEW openrails.freeloader_episodes WITH (security_invoker='true') AS
 WITH win AS (
         SELECT e.merchant_id,
            e.customer_id,
            e.id AS entitlement_id,
            e.entitlement,
            e.source_type,
            e.source_id,
            e.start_at,
            LEAST(COALESCE(e.revoked_at, 'infinity'::timestamp with time zone), COALESCE(e.deleted_at, 'infinity'::timestamp with time zone), COALESCE(e.end_at, 'infinity'::timestamp with time zone)) AS window_end,
            s.status AS sub_status,
            s.next_retry_at,
            GREATEST(s.current_period_ends_at, s.ended_at) AS paid_through,
            p.id AS payment_id,
            p.status AS payment_status,
            COALESCE(( SELECT max(r.purchased_at) AS max
                   FROM openrails.payments r
                  WHERE ((r.merchant_id = e.merchant_id) AND (r.refunded_payment_id = p.id))), p.purchased_at) AS refund_effective_at,
            ( SELECT max(COALESCE(g.ends_at, 'infinity'::timestamp with time zone)) AS max
                   FROM openrails.grants g
                  WHERE ((g.merchant_id = e.merchant_id) AND (g.customer_id = e.customer_id) AND (g.event = 'grant'::text) AND (g.kind = 'entitlement'::text) AND (g.starts_at <= now()) AND ((g.id = e.grant_id) OR ((g.source_id = (e.source_id)::text) AND (((e.source_type = 'subscription'::text) AND (g.source_type = 'subscription'::text)) OR ((e.source_type = 'one_off'::text) AND (g.source_type = 'purchase'::text))))) AND (NOT (EXISTS ( SELECT 1
                           FROM openrails.grants t
                          WHERE ((t.merchant_id = g.merchant_id) AND (t.supersedes_id = g.id) AND (t.event = ANY (ARRAY['revoke'::text, 'expire'::text, 'supersede'::text])))))))) AS grant_covered_until
           FROM ((openrails.entitlements e
             LEFT JOIN openrails.subscriptions s ON (((e.source_type = 'subscription'::text) AND (s.id = e.source_id) AND (s.merchant_id = e.merchant_id))))
             LEFT JOIN openrails.payments p ON (((e.source_type = 'one_off'::text) AND (p.id = e.source_id) AND (p.merchant_id = e.merchant_id))))
          WHERE (e.source_type = ANY (ARRAY['subscription'::text, 'one_off'::text]))
        ), spans AS (
         SELECT w.merchant_id,
            w.customer_id,
            w.entitlement_id,
            w.entitlement,
            w.source_type,
            w.source_id,
            w.start_at,
            w.window_end,
            w.sub_status,
            w.next_retry_at,
            w.paid_through,
            w.payment_id,
            w.payment_status,
            w.refund_effective_at,
            w.grant_covered_until,
            GREATEST(w.start_at,
                CASE
                    WHEN (w.source_type = 'subscription'::text) THEN COALESCE(w.paid_through, '-infinity'::timestamp with time zone)
                    WHEN (w.payment_status = 'completed'::openrails.payment_status) THEN 'infinity'::timestamp with time zone
                    WHEN (w.payment_status = 'refunded'::openrails.payment_status) THEN w.refund_effective_at
                    ELSE '-infinity'::timestamp with time zone
                END, COALESCE(w.grant_covered_until, '-infinity'::timestamp with time zone)) AS unpaid_from,
            LEAST(w.window_end, now()) AS unpaid_until
           FROM win w
        )
 SELECT merchant_id,
    customer_id,
    entitlement_id,
    entitlement,
    source_type,
    source_id,
        CASE
            WHEN ((sub_status = 'past_due'::openrails.subscription_status) AND (next_retry_at IS NOT NULL)) THEN 'sanctioned_dunning'::text
            WHEN (sub_status = 'unknown'::openrails.subscription_status) THEN 'awaiting_verification'::text
            ELSE 'unsanctioned'::text
        END AS cause,
    unpaid_from AS started_at,
    unpaid_until AS ended_at,
    (window_end > now()) AS open,
    ((EXTRACT(epoch FROM (unpaid_until - unpaid_from)) / 86400.0))::double precision AS days
   FROM spans
  WHERE (unpaid_from < unpaid_until);

COMMENT ON VIEW openrails.freeloader_episodes IS '#690 episode analytics: spans of entitlement access NOT covered by payment (subscription paid-through snapshot, completed one_off payment, or a live matching grant). Open episodes (window still granting) end at now(). Causes label sanctioned unpaid access (sanctioned_dunning, awaiting_verification) vs failure (unsanctioned). Approximations: paid-through is the current-period snapshot (renewals overwrite it, healed historical lapses are invisible); coverage is contiguous-from-the-left (uncovered TAIL only); cause reads the sub''s CURRENT state; refund time falls back to the purchase time when no refund row links.';

GRANT SELECT ON TABLE openrails.freeloader_episodes TO openrails_app;

CREATE VIEW openrails.orphaned_episodes WITH (security_invoker='true') AS
 WITH cov AS (
         SELECT s.merchant_id,
            s.customer_id,
            'subscription'::text AS source_type,
            s.id AS source_id,
            s.product_id,
            COALESCE(s.current_period_starts_at, s.started_at) AS cov_start,
            GREATEST(s.current_period_ends_at, s.ended_at) AS cov_end
           FROM (openrails.subscriptions s
             JOIN openrails.products pd ON (((pd.id = s.product_id) AND (pd.merchant_id = s.merchant_id))))
          WHERE ((s.status <> 'pending'::openrails.subscription_status) AND (GREATEST(s.current_period_ends_at, s.ended_at) IS NOT NULL) AND (((pd.entitlements_spec IS NOT NULL) AND (pd.entitlements_spec <> '{}'::jsonb)) OR ((s.entitlements_spec_snapshot IS NOT NULL) AND (s.entitlements_spec_snapshot <> '{}'::jsonb))))
        UNION ALL
         SELECT p.merchant_id,
            p.customer_id,
            'one_off'::text AS text,
            p.id,
            pr.product_id,
            p.purchased_at,
            (p.purchased_at + make_interval(hours => pr.access_duration_hours))
           FROM ((openrails.payments p
             JOIN openrails.prices pr ON (((pr.id = p.price_id) AND (pr.merchant_id = p.merchant_id))))
             JOIN openrails.products pd ON (((pd.id = pr.product_id) AND (pd.merchant_id = p.merchant_id))))
          WHERE ((p.status = 'completed'::openrails.payment_status) AND (p.amount > 0) AND (p.subscription_id IS NULL) AND (pr.access_duration_hours IS NOT NULL) AND (pd.entitlements_spec IS NOT NULL) AND (pd.entitlements_spec <> '{}'::jsonb))
        ), spans AS (
         SELECT c.merchant_id,
            c.customer_id,
            c.source_type,
            c.source_id,
            c.product_id,
            c.cov_start,
            c.cov_end,
            GREATEST(c.cov_start, COALESCE(( SELECT max(LEAST(COALESCE(e.revoked_at, 'infinity'::timestamp with time zone), COALESCE(e.deleted_at, 'infinity'::timestamp with time zone), COALESCE(e.end_at, 'infinity'::timestamp with time zone))) AS max
                   FROM openrails.entitlements e
                  WHERE ((e.merchant_id = c.merchant_id) AND (e.customer_id = c.customer_id) AND (e.source_type = c.source_type) AND (e.source_id = c.source_id) AND (e.start_at <= now()))), '-infinity'::timestamp with time zone)) AS uncovered_from,
            LEAST(c.cov_end, now()) AS uncovered_until
           FROM cov c
        )
 SELECT merchant_id,
    customer_id,
    source_type,
    source_id,
    product_id,
    uncovered_from AS started_at,
    uncovered_until AS ended_at,
    (cov_end > now()) AS open,
    ((EXTRACT(epoch FROM (uncovered_until - uncovered_from)) / 86400.0))::double precision AS days
   FROM spans
  WHERE (uncovered_from < uncovered_until);

COMMENT ON VIEW openrails.orphaned_episodes IS '#690 episode analytics, the mirror of freeloader_episodes: spans where payment coverage existed (subscription paid-through snapshot, or a completed one_off payment with a finite access window for an entitlement-promising product) but no entitlement window covered the time. Open episodes (paid-through still in the future) end at now(). Same approximations: paid-through is the current-period snapshot; window coverage is contiguous-from-the-left (uncovered TAIL only — a wrongly-early revocation shows as the tail from revoked_at to paid-through).';

GRANT SELECT ON TABLE openrails.orphaned_episodes TO openrails_app;
