-- parent: root
-- OpenRails PostgreSQL schema. Objects are authored in the canonical
-- `openrails` schema; the migrator relocates them to the configured schema.
-- AuthKit and River are migrated separately by their own libraries.

SET statement_timeout = '300s';
SET lock_timeout = '10s';
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;

-- btree_gist backs the EXCLUDE constraints on uuid+tstzrange
CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

CREATE SCHEMA IF NOT EXISTS billing;

-- ---------------------------------------------------------------------------
-- Types
-- ---------------------------------------------------------------------------

CREATE TYPE billing.payment_status AS ENUM (
    'pending',
    'completed',
    'failed',
    'refunded'
);

CREATE TYPE billing.subscription_status AS ENUM (
    'pending',
    'active',
    'past_due',
    'awaiting_method',
    'cancelled',
    'unverified'
);
COMMENT ON TYPE billing.subscription_status IS 'or#893: the canonical LOCAL subscription lifecycle. One question: will we attempt to rebill? pending = not started; active/past_due = yes; unknown = provider must tell us (#632); cancelled = never again, with cancel_type carrying why (user|merchant|expired|chargeback). Provider vocabulary is mapped onto this set at the boundary — a remote "expired" becomes cancelled/cancel_type=expired, never a local status.';

-- ---------------------------------------------------------------------------
-- Shared functions
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.current_merchant_id() RETURNS uuid
    LANGUAGE sql STABLE
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
    SELECT NULLIF(current_setting('app.merchant_id', true), '')::uuid
$$;
COMMENT ON FUNCTION billing.current_merchant_id() IS 'The request''s merchant from the app.merchant_id GUC, or NULL when unset. Used only by explicitly scoped SQL and restore transaction guards. Merely setting it does not filter other queries; their merchant predicates are mandatory.';
REVOKE ALL ON FUNCTION billing.current_merchant_id() FROM PUBLIC;

-- Financial facts are immutable to ordinary DML, the schema owner included.
CREATE FUNCTION billing.reject_immutable_billing_fact() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    RAISE EXCEPTION '% is not permitted on immutable billing facts in %', TG_OP, TG_TABLE_NAME USING ERRCODE='23514';
END;
$$;

CREATE FUNCTION billing.guard_billing_fact_columns() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP='DELETE' OR (to_jsonb(NEW)-TG_ARGV) IS DISTINCT FROM (to_jsonb(OLD)-TG_ARGV) THEN
        RAISE EXCEPTION 'immutable billing facts in % cannot be rewritten', TG_TABLE_NAME USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.price_interval_label(p_hours integer, p_auto_renew boolean) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
SELECT CASE
    WHEN NOT coalesce(p_auto_renew, false) OR p_hours IS NULL THEN 'onetime'
    WHEN p_hours = 168 THEN 'weekly'
    WHEN p_hours = 720 THEN 'monthly'
    WHEN p_hours = 2160 THEN 'quarterly'
    WHEN p_hours = 8760 THEN 'yearly'
    WHEN p_hours > 0 AND p_hours % 24 = 0 THEN (p_hours / 24)::text || 'd'
    ELSE p_hours::text || 'h'
END
$$;
COMMENT ON FUNCTION billing.price_interval_label(p_hours integer, p_auto_renew boolean) IS 'Default price-key interval label: exact and collision-free (<n>h, or <n>d on whole days; weekly/monthly/quarterly/yearly only for exactly 168/720/2160/8760 hours).';

CREATE FUNCTION billing.billing_cycle_label(p_hours integer) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
SELECT CASE
    WHEN p_hours IS NULL THEN 'one_time'
    WHEN p_hours < 24 THEN 'hourly'
    WHEN p_hours < 48 THEN 'daily'
    WHEN p_hours < 336 THEN 'weekly'
    WHEN p_hours < 1440 THEN 'monthly'
    WHEN p_hours < 3600 THEN 'quarterly'
    WHEN p_hours < 6480 THEN 'semiannual'
    ELSE 'annual'
END
$$;
COMMENT ON FUNCTION billing.billing_cycle_label(p_hours integer) IS 'Analytics cadence bucket for a price access window.';

CREATE FUNCTION billing.monthly_normalized_amount(p_amount bigint, p_hours integer) RETURNS bigint
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
SELECT CASE
    WHEN p_hours IS NULL OR p_hours <= 0 THEN 0
    WHEN p_hours >= 648 THEN round(p_amount::numeric / greatest(round(p_hours / 730.0), 1))::bigint
    ELSE round(p_amount::numeric * 730.0 / p_hours)::bigint
END
$$;
COMMENT ON FUNCTION billing.monthly_normalized_amount(p_amount bigint, p_hours integer) IS 'The one MRR normalisation: windows of at least 27 days divide by their whole-month count; shorter windows scale by 730 hours per month.';

-- ---------------------------------------------------------------------------
-- Merchants
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.guard_merchant_group_binding() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'billing'
    AS $$
BEGIN
    IF OLD.permission_group_id IS NOT NULL AND OLD.permission_group_id IS DISTINCT FROM NEW.permission_group_id THEN
        RAISE EXCEPTION 'merchant group binding is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.guard_merchant_restore() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'billing'
    AS $$
BEGIN
    IF OLD.retired_at IS NOT NULL AND NEW.retired_at IS DISTINCT FROM OLD.retired_at THEN
        RAISE EXCEPTION 'merchant retirement is irreversible' USING ERRCODE='23514';
    END IF;
    IF NEW.deleted_at IS NULL AND NEW.status='active' THEN

        IF (
        OLD.retired_at IS NOT NULL OR EXISTS (
            SELECT 1 FROM billing.maintenance_runs r
             WHERE r.merchant_id=OLD.id AND r.kind='merchant_purge'
               AND r.affected->>'database_purged'='true'
        )
    ) THEN
        RAISE EXCEPTION 'retired or purged merchant cannot be restored' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION billing.guard_merchant_restore() FROM PUBLIC;

-- One name namespace across live names and aliases. Claims serialize per name
-- on a transaction lock, so a rename's alias and a concurrent claim of the same
-- name cannot both win.
CREATE FUNCTION billing.guard_merchant_name() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'billing'
    AS $$
BEGIN
    IF NEW.deleted_at IS NOT NULL THEN
        IF TG_OP = 'UPDATE' AND OLD.deleted_at IS NULL THEN
            DELETE FROM billing.merchant_slug_aliases WHERE merchant_id = NEW.id;
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.deleted_at IS NULL THEN
        IF OLD.slug = NEW.slug THEN
            RETURN NEW;
        END IF;
        PERFORM pg_advisory_xact_lock(1106, hashtext(least(OLD.slug, NEW.slug)));
        PERFORM pg_advisory_xact_lock(1106, hashtext(greatest(OLD.slug, NEW.slug)));
    ELSE
        PERFORM pg_advisory_xact_lock(1106, hashtext(NEW.slug));
    END IF;
    DELETE FROM billing.merchant_slug_aliases
     WHERE slug = NEW.slug AND (merchant_id = NEW.id OR expires_at <= now());
    IF EXISTS (SELECT 1 FROM billing.merchant_slug_aliases WHERE slug = NEW.slug) THEN
        RAISE EXCEPTION 'merchant name % is another merchant''s former name', NEW.slug
            USING ERRCODE = '23505', CONSTRAINT = 'merchant_slug_aliases_pkey';
    END IF;
    RETURN NEW;
END;
$$;
COMMENT ON FUNCTION billing.guard_merchant_name() IS '#1106: a live merchant name is never another merchant''s unexpired former name; leaving the directory releases a merchant''s former names. Raises 23505 on merchant_slug_aliases_pkey.';
REVOKE ALL ON FUNCTION billing.guard_merchant_name() FROM PUBLIC;

CREATE TABLE billing.merchants (
    id uuid DEFAULT uuidv7() NOT NULL,
    slug text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    permission_group_id text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    deleted_at timestamp with time zone,
    display_name text,
    api_host text,
    retired_at timestamp with time zone,
    group_release_completed_at timestamp with time zone,
    catalog_revision bigint NOT NULL DEFAULT 0 CHECK (catalog_revision >= 0),
    slug_changed_at timestamp with time zone,
    CONSTRAINT merchants_status_check CHECK ((status = ANY (ARRAY['active'::text, 'deleted'::text])))
);
COMMENT ON TABLE billing.merchants IS 'Merchant / billing-namespace directory: a dumb billing bucket (whose books a row goes on). GLOBAL (control-plane) table, not tenant-scoped. Carries ONLY billing/money-rail state, NO auth. Merchants are registered explicitly; there is no default merchant. Global by design: it IS the tenant directory — the scope, not a scoped row.';
COMMENT ON COLUMN billing.merchants.slug IS 'The merchant''s public name (#1106): unique among live rows and never equal to another merchant''s unexpired former name (openrails.merchant_slug_aliases). OpenRails owns it; AuthKit groups carry no name.';
COMMENT ON COLUMN billing.merchants.permission_group_id IS 'The merchant''s own AuthKit permission-group id (#567): a merchant IS a top-level `merchant` group, child of `root`. Bare `text`, NO FK into the auth schema (#544 portability guard). NULL for a host-owned merchant without a control plane.';
COMMENT ON COLUMN billing.merchants.display_name IS 'human-readable merchant name for end-user display / invoices; NULL = fall back to slug.';
COMMENT ON COLUMN billing.merchants.api_host IS 'Canonical Host-header value this merchant resolves from (#734), e.g. "api.acme.example". NULL = no Host resolution for this merchant. Lowercase, no scheme/port.';
COMMENT ON COLUMN billing.merchants.slug_changed_at IS 'When the merchant was last renamed (#1106); NULL if never. The rename interval counts from here.';

ALTER TABLE ONLY billing.merchants
    ADD CONSTRAINT merchants_pkey PRIMARY KEY (id);

CREATE INDEX idx_merchants_pending_group_release ON billing.merchants USING btree (retired_at, id) WHERE ((retired_at IS NOT NULL) AND (group_release_completed_at IS NULL));
CREATE UNIQUE INDEX uq_merchants_api_host ON billing.merchants USING btree (api_host) WHERE ((api_host IS NOT NULL) AND (deleted_at IS NULL));
CREATE UNIQUE INDEX uq_merchants_permission_group_id ON billing.merchants USING btree (permission_group_id) WHERE (permission_group_id IS NOT NULL);
CREATE UNIQUE INDEX uq_merchants_live_slug ON billing.merchants USING btree (slug) WHERE (deleted_at IS NULL);

CREATE TRIGGER guard_merchant_restore BEFORE UPDATE ON billing.merchants FOR EACH ROW EXECUTE FUNCTION billing.guard_merchant_restore();
CREATE TRIGGER immutable_merchant_group_binding BEFORE UPDATE OF permission_group_id ON billing.merchants FOR EACH ROW EXECUTE FUNCTION billing.guard_merchant_group_binding();
CREATE TRIGGER guard_merchant_name BEFORE INSERT OR UPDATE OF slug, deleted_at ON billing.merchants FOR EACH ROW EXECUTE FUNCTION billing.guard_merchant_name();

CREATE TABLE billing.merchant_slug_aliases (
    slug text NOT NULL,
    merchant_id uuid NOT NULL,
    expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT merchant_slug_aliases_pkey PRIMARY KEY (slug),
    CONSTRAINT merchant_slug_aliases_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.merchant_slug_aliases IS '#1106: former merchant names. An unexpired alias forwards to its merchant and blocks every other claim of the name; expires_at NULL keeps it forever. Written by a rename, removed when its merchant takes the name back, when it expires and is claimed, or when its merchant leaves the directory.';

CREATE INDEX idx_merchant_slug_aliases_merchant ON billing.merchant_slug_aliases USING btree (merchant_id);

CREATE TABLE billing.merchant_api_host_claims (
    merchant_id uuid NOT NULL,
    api_host text NOT NULL,
    token text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT merchant_api_host_claims_pkey PRIMARY KEY (merchant_id),
    CONSTRAINT merchant_api_host_claims_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.merchant_api_host_claims IS '#1107: a merchant''s unproven api_host claim, one per merchant. The token must appear in a TXT record at _openrails-challenge.<api_host> before the host binds to merchants.api_host. Routes nothing.';

-- ---------------------------------------------------------------------------
-- Maintenance runs, destructive controls and billing restore
-- ---------------------------------------------------------------------------

CREATE TABLE billing.destructive_action_switch (
    id uuid DEFAULT uuidv7() NOT NULL,
    singleton boolean DEFAULT true NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    updated_by text,
    reason text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_destructive_action_switch_singleton CHECK ((singleton = true))
);
COMMENT ON TABLE billing.destructive_action_switch IS 'Global by design: instance-level operator kill switch for destructive convergence (#836), not tenant data. One row. Read from the no-GUC background connections the intent runner and sweep scheduler use, so it cannot be defeated by the connection scope it polices. Default disabled: a fresh deployment cancels nothing until an operator arms it.';

ALTER TABLE ONLY billing.destructive_action_switch
    ADD CONSTRAINT destructive_action_switch_pkey PRIMARY KEY (id);

CREATE UNIQUE INDEX uq_destructive_action_switch_singleton ON billing.destructive_action_switch USING btree (singleton);

INSERT INTO billing.destructive_action_switch (enabled, reason)
VALUES (false, 'default safe (#836): arm deliberately once the first pull''s findings have been reviewed');

CREATE TABLE billing.merchant_destructive_policy (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    destructive_actions_enabled boolean DEFAULT true CONSTRAINT merchant_destructive_policy_destructive_actions_enable_not_null NOT NULL,
    enforce_armed_at timestamp with time zone,
    first_pull_completed_at timestamp with time zone,
    updated_by text,
    reason text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
COMMENT ON TABLE billing.merchant_destructive_policy IS '#836/#835 per-merchant destructive-action policy: destructive_actions_enabled is the per-merchant emergency stop (the instance switch in destructive_action_switch gates it globally); enforce_armed_at is the first-enforce gate — NULL means the merchant''s provider pull runs advisory (findings only, zero mutations) until an operator reviews the first pull and arms it.';
COMMENT ON COLUMN billing.merchant_destructive_policy.enforce_armed_at IS '#835: NULL = advisory-only pulls for this merchant. Absence of a row is the same as NULL, so a newly onboarded merchant is surveyed before it is enforced.';

ALTER TABLE ONLY billing.merchant_destructive_policy
    ADD CONSTRAINT merchant_destructive_policy_pkey PRIMARY KEY (id);

CREATE UNIQUE INDEX uq_merchant_destructive_policy_merchant ON billing.merchant_destructive_policy USING btree (merchant_id);

ALTER TABLE ONLY billing.merchant_destructive_policy
    ADD CONSTRAINT merchant_destructive_policy_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE FUNCTION billing.billing_restore_active(p_merchant uuid) RETURNS boolean
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'billing', 'pg_temp'
    AS $$
BEGIN
    IF p_merchant IS DISTINCT FROM billing.current_merchant_id() THEN RETURN false; END IF;
    RETURN EXISTS (SELECT 1 FROM billing.maintenance_runs r
        WHERE r.merchant_id=p_merchant AND r.kind='billing_restore' AND r.status='running'
        AND r.id::text=current_setting('app.billing_restore_id',true)
        AND r.xmin=pg_current_xact_id_if_assigned()::xid);
END;
$$;
REVOKE ALL ON FUNCTION billing.billing_restore_active(uuid) FROM PUBLIC;

CREATE FUNCTION billing.check_billing_restore_ledger(p_merchant uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF p_merchant IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    IF EXISTS (SELECT 1 FROM billing.ledger_accounts a
        LEFT JOIN (SELECT credit_account_id id,sum(amount) amount FROM billing.ledger_transfers WHERE merchant_id=p_merchant GROUP BY credit_account_id) c ON c.id=a.id
        LEFT JOIN (SELECT debit_account_id id,sum(amount) amount FROM billing.ledger_transfers WHERE merchant_id=p_merchant GROUP BY debit_account_id) d ON d.id=a.id
        WHERE a.merchant_id=p_merchant AND (a.credits_posted<>coalesce(c.amount,0) OR a.debits_posted<>coalesce(d.amount,0)))
       OR EXISTS (SELECT currency FROM billing.ledger_accounts WHERE merchant_id=p_merchant GROUP BY currency HAVING sum(credits_posted::numeric-debits_posted::numeric)<>0)
       OR EXISTS (SELECT 1 FROM billing.ledger_transfers t JOIN billing.ledger_accounts d ON d.id=t.debit_account_id
           JOIN billing.ledger_accounts c ON c.id=t.credit_account_id WHERE t.merchant_id=p_merchant
           AND (d.currency<>t.currency OR c.currency<>t.currency OR d.merchant_id<>p_merchant OR c.merchant_id<>p_merchant
                OR (d.customer_id IS NOT NULL AND d.customer_id IS DISTINCT FROM t.customer_id)
                OR (c.customer_id IS NOT NULL AND c.customer_id IS DISTINCT FROM t.customer_id))) THEN
        RAISE EXCEPTION 'billing restore ledger integrity mismatch' USING ERRCODE='23514';
    END IF;
END;
$$;
REVOKE ALL ON FUNCTION billing.check_billing_restore_ledger(uuid) FROM PUBLIC;

CREATE FUNCTION billing.guard_billing_restore_receipt() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE item record; occupied boolean;
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.kind='billing_restore' THEN
            RAISE EXCEPTION 'billing restore receipts are immutable' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.kind<>'billing_restore' AND (TG_OP='INSERT' OR OLD.kind<>'billing_restore') THEN RETURN NEW; END IF;
    IF NEW.merchant_id IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    -- The merchant row is the serialization point used by begin_billing_restore
    -- and by FK-backed first writes. No database-owner exemption or GUC-only
    -- permission can create a receipt for an occupied destination.
    PERFORM 1 FROM billing.merchants WHERE id=NEW.merchant_id AND status='active' AND deleted_at IS NULL FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'billing restore merchant missing or inactive' USING ERRCODE='P0002'; END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.status<>'running' OR NEW.finished_at IS NOT NULL OR NEW.summary IS NOT NULL THEN
            RAISE EXCEPTION 'billing restore receipts must begin running and unfinished' USING ERRCODE='23514';
        END IF;
        FOR item IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
            JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='merchant_id' AND NOT a.attisdropped
            WHERE n.nspname=TG_TABLE_SCHEMA AND c.relkind IN ('r','p')
              AND c.relname = ANY(ARRAY[
                  'account_updater_batches',
                  'admission_denials_hourly',
                  'admission_operations',
                  'billing_policies',
                  'billing_policy_bindings',
                  'catalog_applications',
                  'catalog_meters',
                  'catalog_rate_cards',
                  'catalogs',
                  'checkout_sessions',
                  'credential_publications',
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_invoice_profiles',
                  'customers',
                  'dashboard_configs',
                  'destructive_action_switch',
                  'destructive_run_before_images',
                  'entitlements',
                  'grants',
                  'host_outbox',
                  'invoice_items',
                  'invoice_payments',
                  'invoices',
                  'invoker_spend_limits',
                  'ledger_accounts',
                  'ledger_transfers',
                  'maintenance_runs',
                  'merchant_configurations',
                  'merchant_configuration_applications',
                  'merchant_deks',
                  'merchant_destructive_policy',
                  'merchant_secrets',
                  'merchant_webhooks',
                  'merchants',
                  'metered_rating_watermarks',
                  'money_settings',
                  'notifications',
                  'operation_authorizations',
                  'payment_methods',
                  'payments',
                  'price_key_movements',
                  'price_psp_bindings',
                  'prices',
                  'product_archive_operations',
                  'products',
                  'provider_billing_observations',
                  'provider_billing_qualifications',
                  'psps',
                  'rail_customer_accounts',
                  'rail_intents',
                  'rail_mutation_logs',
                  'rail_refresh_watermarks',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'reprice_batches',
                  'solana_subscriptions',
                  'subscription_reprices',
                  'subscription_status_transitions',
                  'subscriptions',
                  'usage_events',
                  'webhook_events',
                  'webhook_health',
                  'webhook_health_daily',
                  'worker_state']::text[])
        LOOP
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.%I WHERE merchant_id=$1)',TG_TABLE_SCHEMA,item.relname)
                INTO occupied USING NEW.merchant_id;
            IF occupied THEN RAISE EXCEPTION 'billing restore destination is not empty: %',item.relname USING ERRCODE='55000'; END IF;
        END LOOP;
    ELSE
        IF OLD.kind<>'billing_restore' OR NEW.kind<>'billing_restore'
           OR OLD.status<>'running' OR NEW.status<>'completed'
           OR NEW.finished_at IS NULL
           OR (to_jsonb(NEW)-ARRAY['status','finished_at','summary','run_class']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['status','finished_at','summary','run_class'])
           OR NOT EXISTS (SELECT 1 FROM billing.maintenance_runs r WHERE r.id=OLD.id AND r.merchant_id=OLD.merchant_id
               AND r.xmin=pg_current_xact_id_if_assigned()::xid)
           OR OLD.id::text IS DISTINCT FROM current_setting('app.billing_restore_id',true)
           OR NEW.summary->>'digest' IS NULL OR NEW.summary->>'digest' !~ '^[0-9a-f]{64}$'
           OR jsonb_typeof(NEW.summary->'rows') IS DISTINCT FROM 'number'
           OR (NEW.summary->>'rows')::numeric < 0
           OR (NEW.summary->>'rows')::numeric <> trunc((NEW.summary->>'rows')::numeric) THEN
            RAISE EXCEPTION 'invalid billing restore receipts transition' USING ERRCODE='23514';
        END IF;
        PERFORM billing.check_billing_restore_ledger(NEW.merchant_id);
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.require_finished_billing_restore() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.kind='billing_restore' THEN
        IF NOT EXISTS (SELECT 1 FROM billing.maintenance_runs WHERE id=NEW.id AND merchant_id=NEW.merchant_id AND status='completed') THEN
            RAISE EXCEPTION 'unfinished billing restore receipts cannot commit' USING ERRCODE='23514';
        END IF;
        PERFORM billing.check_billing_restore_ledger(NEW.merchant_id);
    END IF;
    RETURN NULL;
END;
$$;

CREATE FUNCTION billing.begin_billing_restore(p_merchant uuid) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE receipt uuid;
BEGIN
    IF p_merchant IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    PERFORM 1 FROM billing.merchants WHERE id=p_merchant AND status='active' AND deleted_at IS NULL FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'billing restore merchant missing or inactive' USING ERRCODE='P0002'; END IF;
    SELECT id INTO receipt FROM billing.maintenance_runs WHERE merchant_id=p_merchant AND kind='billing_restore' AND status='completed';
    IF receipt IS NOT NULL THEN RETURN receipt; END IF;
    INSERT INTO billing.maintenance_runs(merchant_id,kind,actor) VALUES(p_merchant,'billing_restore','merchantarchive') RETURNING id INTO receipt;
    PERFORM set_config('app.billing_restore_id',receipt::text,true);
    RETURN receipt;
END;
$$;
REVOKE ALL ON FUNCTION billing.begin_billing_restore(uuid) FROM PUBLIC;

CREATE FUNCTION billing.finish_billing_restore(p_merchant uuid,p_digest text,p_rows bigint) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NOT billing.billing_restore_active(p_merchant) OR p_digest IS NULL OR p_rows IS NULL OR p_digest !~ '^[0-9a-f]{64}$' OR p_rows<0 THEN
        RAISE EXCEPTION 'invalid billing restore finalization' USING ERRCODE='23514';
    END IF;
    PERFORM billing.check_billing_restore_ledger(p_merchant);
    UPDATE billing.maintenance_runs SET status='completed',finished_at=now(),summary=jsonb_build_object('digest',p_digest,'rows',p_rows)
        WHERE merchant_id=p_merchant AND kind='billing_restore' AND id::text=current_setting('app.billing_restore_id',true);
END;
$$;
REVOKE ALL ON FUNCTION billing.finish_billing_restore(uuid,text,bigint) FROM PUBLIC;

CREATE TABLE billing.maintenance_runs (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    kind text NOT NULL,
    actor text DEFAULT '' NOT NULL,
    psp_id uuid,
    mode text DEFAULT '' NOT NULL,
    rails text[] DEFAULT '{}' NOT NULL,
    window_since timestamp with time zone,
    window_until timestamp with time zone,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone,
    status text DEFAULT 'running' NOT NULL,
    dry_run boolean DEFAULT false NOT NULL,
    coverage jsonb,
    expected_rows bigint,
    affected jsonb,
    reversed_at timestamp with time zone,
    reversed_by text,
    note text,
    summary jsonb,
    error text,
    inventory_manifest jsonb,
    inventory_total_rows bigint,
    run_class text GENERATED ALWAYS AS (CASE WHEN kind = 'reconciliation' THEN 'observation' WHEN kind = 'purge_inventory' THEN 'inventory' WHEN kind = 'billing_restore' THEN 'restore' ELSE 'destructive' END) STORED NOT NULL,
    CONSTRAINT maintenance_runs_expected_rows CHECK (expected_rows IS NULL OR expected_rows >= 0),
    CONSTRAINT maintenance_runs_status CHECK (status IN ('running','completed','failed','reversed')),
    CONSTRAINT maintenance_runs_shape CHECK ((
        (kind = 'reconciliation' AND mode IN ('advisory','enforce')
         AND status IN ('running','completed','failed') AND psp_id IS NULL
         AND NOT dry_run AND coverage IS NULL AND expected_rows IS NULL AND affected IS NULL
         AND reversed_at IS NULL AND reversed_by IS NULL AND inventory_manifest IS NULL AND inventory_total_rows IS NULL)
        OR (kind IN ('prune','converge_enforce','merchant_purge') AND btrim(actor) <> ''
         AND mode = '' AND cardinality(rails) = 0 AND window_since IS NULL AND window_until IS NULL
         AND summary IS NULL AND error IS NULL AND inventory_manifest IS NULL AND inventory_total_rows IS NULL)
        OR (kind = 'billing_restore' AND actor = 'merchantarchive' AND status IN ('running','completed')
         AND mode = '' AND cardinality(rails)=0 AND psp_id IS NULL AND NOT dry_run
         AND window_since IS NULL AND window_until IS NULL AND coverage IS NULL AND expected_rows IS NULL
         AND affected IS NULL AND reversed_at IS NULL AND reversed_by IS NULL AND note IS NULL AND error IS NULL
         AND inventory_manifest IS NULL AND inventory_total_rows IS NULL
         AND ((status='running' AND finished_at IS NULL AND summary IS NULL)
              OR (status='completed' AND finished_at IS NOT NULL AND jsonb_typeof(summary)='object'
                  AND summary ?& ARRAY['digest','rows'] AND jsonb_typeof(summary->'digest')='string'
                  AND jsonb_typeof(summary->'rows')='number' AND summary->>'digest' ~ '^[0-9a-f]{64}$' AND (summary->>'rows')::bigint >= 0)))
        OR (kind = 'purge_inventory' AND status = 'completed' AND finished_at IS NOT NULL
         AND mode = '' AND cardinality(rails) = 0 AND psp_id IS NULL
         AND window_since IS NULL AND window_until IS NULL AND NOT dry_run
         AND coverage IS NULL AND expected_rows IS NULL AND affected IS NULL
         AND reversed_at IS NULL AND reversed_by IS NULL AND summary IS NULL AND error IS NULL
         AND inventory_manifest IS NOT NULL AND jsonb_typeof(inventory_manifest) = 'object'
         AND jsonb_typeof(inventory_manifest->'total_rows') = 'number'
         AND inventory_total_rows IS NOT NULL AND inventory_total_rows >= 0
         AND inventory_total_rows = (inventory_manifest->>'total_rows')::bigint)
    ) IS TRUE)
);
COMMENT ON TABLE billing.maintenance_runs IS 'Typed maintenance run headers: reconciliation observations, reversible destructive work, and immutable purge inventories. Each kind has explicit columns and constraints; before-images remain in destructive_run_before_images.';
COMMENT ON COLUMN billing.maintenance_runs.coverage IS 'The coverage proof authorizing a destructive run, retained unchanged for audit and undo.';
COMMENT ON COLUMN billing.maintenance_runs.expected_rows IS 'The operator-confirmed or planned affected row count.';
COMMENT ON COLUMN billing.maintenance_runs.inventory_manifest IS 'Purge row counts, secret names, and omitted resources. Not a backup and never an undo image.';
COMMENT ON COLUMN billing.maintenance_runs.run_class IS 'Derived from the immutable kind. Child foreign keys include it, so findings can reference only observation runs and stamped rows, intents and before-images only destructive runs.';

ALTER TABLE ONLY billing.maintenance_runs ADD CONSTRAINT maintenance_runs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.maintenance_runs ADD CONSTRAINT maintenance_runs_merchant_id_id_class_key UNIQUE (merchant_id,id,run_class);

CREATE INDEX maintenance_runs_merchant_kind_started ON billing.maintenance_runs (merchant_id,kind,started_at DESC);
CREATE INDEX maintenance_runs_pending_secret_cleanup_idx ON billing.maintenance_runs (id)
    WHERE kind='merchant_purge' AND status IN ('running','failed') AND affected->>'database_purged'='true' AND coverage ? 'secret_cleanup';
-- One receipt, on the maintenance ledger, protects the narrow
-- restore-only trigger suppression. An application-set GUC alone does nothing.
CREATE UNIQUE INDEX uq_maintenance_billing_restore ON billing.maintenance_runs(merchant_id)
    WHERE kind='billing_restore';

ALTER TABLE ONLY billing.maintenance_runs ADD CONSTRAINT maintenance_runs_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TRIGGER guard_billing_restore_receipt BEFORE INSERT OR UPDATE OR DELETE ON billing.maintenance_runs
    FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_restore_receipt();
CREATE CONSTRAINT TRIGGER require_finished_billing_restore AFTER INSERT OR UPDATE ON billing.maintenance_runs
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION billing.require_finished_billing_restore();
CREATE TRIGGER immutable_maintenance_run_facts BEFORE UPDATE OR DELETE ON billing.maintenance_runs
FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('finished_at','status','summary','error','affected','reversed_at','reversed_by','note','run_class');
CREATE TRIGGER immutable_maintenance_runs_truncate BEFORE TRUNCATE ON billing.maintenance_runs
EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.destructive_run_before_images (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    destructive_run_id uuid NOT NULL,
    table_name text NOT NULL,
    row_id uuid NOT NULL,
    before jsonb NOT NULL,
    captured_at timestamp with time zone DEFAULT now() NOT NULL,
    restored_at timestamp with time zone,
    destructive_run_class text GENERATED ALWAYS AS ('destructive') STORED NOT NULL,
    CONSTRAINT chk_destructive_run_before_images_table CHECK ((table_name = ANY (ARRAY['subscriptions'::text, 'entitlements'::text])))
);
COMMENT ON TABLE billing.destructive_run_before_images IS 'or#859 tier 1: the row as it stood immediately before a destructive run overwrote it. or#858''s soft-delete stamp reverses DELETEs; this reverses UPDATEs — which is the damage the empty-roster mass-cancellation actually did. One image per (run, table, row); FK-pinned to exactly one run.';
COMMENT ON COLUMN billing.destructive_run_before_images.before IS 'to_jsonb(row) verbatim, captured server-side inside the run. Complete evidence; the restore reads an explicit typed column projection out of it rather than rewriting the whole row.';
COMMENT ON COLUMN billing.destructive_run_before_images.restored_at IS 'When the reverse replayed this image. NULL after a completed reversal means the image was captured as evidence but deliberately never replayed: entitlement rows are RECOMPUTED from the append-only grant log by Converge, never restored (or#859 §3.3 / Class D). Restoring one directly could make it disagree with its grant, which recomputation cannot.';

ALTER TABLE ONLY billing.destructive_run_before_images
    ADD CONSTRAINT destructive_run_before_images_pkey PRIMARY KEY (id);

CREATE UNIQUE INDEX uq_destructive_run_before_images_identity ON billing.destructive_run_before_images USING btree (merchant_id, destructive_run_id, table_name, row_id);

ALTER TABLE ONLY billing.destructive_run_before_images
    ADD CONSTRAINT destructive_run_before_images_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.destructive_run_before_images
    ADD CONSTRAINT destructive_run_before_images_run_fk FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs(merchant_id, id, run_class) ON DELETE RESTRICT;

CREATE TRIGGER immutable_destructive_before_images BEFORE UPDATE OR DELETE ON billing.destructive_run_before_images
FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('restored_at','destructive_run_class');

COMMENT ON INDEX billing.uq_destructive_run_before_images_identity IS 'ID-11: merchant-led. One image per (run, table, row) WITHIN a merchant — the second capture inside a run is the run''s own later write, not the state it inherited, and must never displace the first (the capture is ON CONFLICT DO NOTHING for that reason). Also serves the by-run reads of the reverse, so no separate (merchant_id, destructive_run_id) index is kept.';

CREATE TABLE billing.worker_state (
    worker_kind text NOT NULL,
    cursor_merchant_id uuid,
    cursor_version bigint DEFAULT 0 NOT NULL,
    registered_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    expected_period_seconds bigint,
    last_success_at timestamp with time zone,
    last_error_at timestamp with time zone,
    last_error text,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    last_alerted_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);
COMMENT ON TABLE billing.worker_state IS 'Global by design: operator-global worker health and fair sweep progress. Health and cursor writers update only their own fields. NULL cursor starts at the beginning; otherwise restart resumes after cursor_merchant_id.';
COMMENT ON COLUMN billing.worker_state.cursor_version IS 'Opaque compare-and-swap token for fair-sweep cursor saves: +1 per applied save, never touched by health writes, independent of any clock.';
COMMENT ON COLUMN billing.worker_state.registered_at IS 'First time this kind was seeded (deploy that introduced it) — anchors the never-succeeded-since-deploy alert.';
COMMENT ON COLUMN billing.worker_state.expected_period_seconds IS 'Declared periodic cadence captured at registration; NULL/0 = on-demand kind (no staleness alerting).';
COMMENT ON COLUMN billing.worker_state.last_error IS 'Most recent work error, truncated by the writer.';
COMMENT ON COLUMN billing.worker_state.last_alerted_at IS 'When the health checker last raised a repair alert for this kind (dedup/re-alert pacing).';

ALTER TABLE ONLY billing.worker_state
    ADD CONSTRAINT worker_state_pkey PRIMARY KEY (worker_kind);

-- ---------------------------------------------------------------------------
-- Merchant configuration and secrets
-- ---------------------------------------------------------------------------

-- Every metadata writer participates in the same merchant lock, including
-- individual API changes and direct modules. Directory UPDATE already takes it.
CREATE FUNCTION billing.lock_merchant_configuration_write() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE mid uuid;
BEGIN
 IF TG_OP='DELETE' THEN mid:=OLD.merchant_id; ELSE mid:=NEW.merchant_id; END IF;
 PERFORM 1 FROM billing.merchants WHERE id=mid FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'merchant metadata requires a merchant' USING ERRCODE='P0002'; END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END $$;

CREATE FUNCTION billing.guard_catalog_application_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'catalog application receipts are immutable' USING ERRCODE='23514';
END $$;

CREATE TABLE billing.merchant_configurations (
    merchant_id uuid NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
COMMENT ON TABLE billing.merchant_configurations IS 'One merchant-scoped JSON configuration row. Missing keys use service defaults.';
COMMENT ON COLUMN billing.merchant_configurations.config IS 'JSONB merchant config. delegated_invoker_wasted_spend_windows is an array of {key, window_seconds, limit}; amount values use the request currency internal precision.';

ALTER TABLE ONLY billing.merchant_configurations
    ADD CONSTRAINT merchant_configurations_pkey PRIMARY KEY (merchant_id);

ALTER TABLE ONLY billing.merchant_configurations
    ADD CONSTRAINT merchant_configurations_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id);

CREATE TRIGGER lock_merchant_configuration BEFORE INSERT OR UPDATE OR DELETE ON billing.merchant_configurations FOR EACH ROW EXECUTE FUNCTION billing.lock_merchant_configuration_write();

-- Metadata applications are local transactions; provider publication is separate.
CREATE TABLE billing.merchant_configuration_applications (
 merchant_id uuid NOT NULL REFERENCES billing.merchants(id) ON DELETE RESTRICT,
 application_id text NOT NULL CHECK (length(application_id) BETWEEN 1 AND 128),
 request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256)=32),
 result jsonb NOT NULL CHECK (octet_length(result::text)<=16384),
 applied_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (merchant_id,application_id)
);

CREATE TRIGGER immutable_merchant_configuration_application BEFORE UPDATE OR DELETE
 ON billing.merchant_configuration_applications FOR EACH ROW
 EXECUTE FUNCTION billing.guard_catalog_application_receipt();

CREATE TABLE billing.merchant_deks (
    merchant_id uuid NOT NULL,
    wrapped_dek bytea NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);
COMMENT ON TABLE billing.merchant_deks IS 'Wrapped per-merchant Data Encryption Keys for envelope encryption-at-rest (issue #227). wrapped_dek = merchant DEK sealed with the master key (AES-256-GCM, nonce||ct||tag). Master key lives in config/env (self-hosted) or KMS (production), never in the DB. Merchant-owned; queries carry explicit merchant predicates.';
COMMENT ON COLUMN billing.merchant_deks.wrapped_dek IS 'AES-256-GCM(master_key, merchant_dek): nonce(12) || ciphertext(32) || tag(16).';

ALTER TABLE ONLY billing.merchant_deks
    ADD CONSTRAINT pk_merchant_deks PRIMARY KEY (merchant_id);

ALTER TABLE ONLY billing.merchant_deks
    ADD CONSTRAINT merchant_deks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TABLE billing.merchant_secrets (
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    value text NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);
COMMENT ON TABLE billing.merchant_secrets IS 'DB-backed per-merchant secret store (issue #225). Namespaced by (merchant_id, name). The Vault-backed store keeps the same addressing but holds values in Vault. Merchant-owned; queries carry explicit merchant predicates.';

ALTER TABLE ONLY billing.merchant_secrets
    ADD CONSTRAINT pk_merchant_secrets PRIMARY KEY (merchant_id, name);

ALTER TABLE ONLY billing.merchant_secrets
    ADD CONSTRAINT merchant_secrets_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

-- Credential publication receipts contain identities and exact references only.
CREATE TABLE billing.credential_publications (
 merchant_id uuid NOT NULL REFERENCES billing.merchants(id) ON DELETE RESTRICT,
 operation_id uuid NOT NULL,
 rail text NOT NULL,
 environment text NOT NULL,
 account_id text NOT NULL,
 expected_revision bigint NOT NULL CHECK (expected_revision >= 0),
 request_metadata jsonb NOT NULL,
 state text NOT NULL DEFAULT 'staging' CHECK (state IN ('staging','published')),
 result jsonb,
 created_at timestamptz NOT NULL DEFAULT now(),
 published_at timestamptz,
 PRIMARY KEY (merchant_id,operation_id)
);

CREATE TABLE billing.merchant_webhooks (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    destination_host text NOT NULL,
    secret_version integer NOT NULL CHECK (secret_version > 0),
    format text DEFAULT 'generic'::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT merchant_webhooks_format_check CHECK ((format = ANY (ARRAY['generic'::text, 'discord'::text, 'slack'::text])))
);
COMMENT ON TABLE billing.merchant_webhooks IS '#736 operator-configured OUTBOUND alert sinks. format shapes the POST body: generic=our alert JSON, discord={content}, slack={text}. NOT the inbound provider-webhook ingestion surface.';

ALTER TABLE ONLY billing.merchant_webhooks
    ADD CONSTRAINT merchant_webhooks_pkey PRIMARY KEY (id);

CREATE INDEX merchant_webhooks_merchant_idx ON billing.merchant_webhooks USING btree (merchant_id);

ALTER TABLE ONLY billing.merchant_webhooks
    ADD CONSTRAINT merchant_webhooks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TABLE billing.dashboard_configs (
    merchant_id uuid NOT NULL,
    layout jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_by text
);
COMMENT ON TABLE billing.dashboard_configs IS '#741 per-merchant dashboard widget layout: [{id, title, viz(stat|line|area|bar|donut|table), query(#733 body), grid{x,y,w,h}}]. Absent row = seeded default template (in code, not DB).';
COMMENT ON COLUMN billing.dashboard_configs.updated_by IS 'acting principal (user id) of the last PUT; informational.';

ALTER TABLE ONLY billing.dashboard_configs
    ADD CONSTRAINT dashboard_configs_pkey PRIMARY KEY (merchant_id);

ALTER TABLE ONLY billing.dashboard_configs
    ADD CONSTRAINT dashboard_configs_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

-- ---------------------------------------------------------------------------
-- Customers
-- ---------------------------------------------------------------------------

CREATE TABLE billing.customers (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    issuer text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL
);
COMMENT ON TABLE billing.customers IS 'OpenRails payable identity. Customer identity is merchant_id plus the host/AuthKit stable UUID subject; id is that payable UUID. issuer is audit/last-seen source only.';
COMMENT ON COLUMN billing.customers.issuer IS 'Audit/last-seen source issuer for delegated/remote customer touches. Not part of customer identity.';

ALTER TABLE ONLY billing.customers
    ADD CONSTRAINT customers_pkey PRIMARY KEY (merchant_id, id);

CREATE INDEX idx_customers_merchant ON billing.customers USING btree (merchant_id);
CREATE INDEX idx_customers_id_merchant ON billing.customers USING btree (id, merchant_id);

ALTER TABLE ONLY billing.customers
    ADD CONSTRAINT customers_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id);

CREATE TABLE billing.customer_invoice_profiles (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    net_terms_days integer DEFAULT 0 NOT NULL,
    collection_method text DEFAULT 'charge_automatically'::text NOT NULL,
    po_number text,
    tax jsonb DEFAULT '{}'::jsonb NOT NULL,
    billing_contacts jsonb DEFAULT '[]'::jsonb NOT NULL,
    memo text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT customer_invoice_profiles_collection_method_chk CHECK ((collection_method = ANY (ARRAY['charge_automatically'::text, 'send_invoice'::text]))),
    CONSTRAINT customer_invoice_profiles_net_terms_chk CHECK ((net_terms_days >= 0))
);
COMMENT ON TABLE billing.customer_invoice_profiles IS '#798 per-payer enterprise invoicing profile: net-N terms, collection method (charge_automatically | send_invoice for manual remittance) and document fields (PO, tax, contacts) snapshotted onto invoices at finalize.';

ALTER TABLE ONLY billing.customer_invoice_profiles
    ADD CONSTRAINT customer_invoice_profiles_pkey PRIMARY KEY (merchant_id, customer_id);

ALTER TABLE ONLY billing.customer_invoice_profiles
    ADD CONSTRAINT customer_invoice_profiles_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE CASCADE;
ALTER TABLE ONLY billing.customer_invoice_profiles
    ADD CONSTRAINT customer_invoice_profiles_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TABLE billing.customer_delinquency (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    state text DEFAULT 'current'::text NOT NULL,
    overdue_since timestamp with time zone,
    entered_at timestamp with time zone DEFAULT now() NOT NULL,
    overdue_amount bigint DEFAULT 0 NOT NULL,
    overdue_invoices bigint DEFAULT 0 NOT NULL,
    transition_seq bigint DEFAULT 0 NOT NULL,
    evaluated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT customer_delinquency_amount_chk CHECK (((overdue_amount >= 0) AND (overdue_invoices >= 0))),
    CONSTRAINT customer_delinquency_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text))),
    CONSTRAINT customer_delinquency_since_chk CHECK ((((state = 'current'::text) AND (overdue_since IS NULL)) OR ((state <> 'current'::text) AND (overdue_since IS NOT NULL)))),
    CONSTRAINT customer_delinquency_state_chk CHECK ((state = ANY (ARRAY['current'::text, 'grace'::text, 'delinquent'::text])))
);
COMMENT ON TABLE billing.customer_delinquency IS 'or#878 per-(merchant, payer, currency) arrears delinquency state: current -> grace -> delinquent, derived from overdue open receivables against the merchant''s declared grace window and amount floor. A projection of invoice truth; only the transition watermarks (entered_at, transition_seq) are not recomputable. Delinquency NEVER revokes an entitlement — it refuses new spend at admission and emits a host_outbox signal; the operator owns the shutoff.';
COMMENT ON COLUMN billing.customer_delinquency.overdue_since IS 'The oldest overdue due_at behind this state — the clock the grace window is measured on, not the moment we noticed.';
COMMENT ON COLUMN billing.customer_delinquency.transition_seq IS 'Bumped only when state changes; the idempotency coordinate of the emitted host_outbox row.';

ALTER TABLE ONLY billing.customer_delinquency
    ADD CONSTRAINT customer_delinquency_pkey PRIMARY KEY (merchant_id, customer_id, currency);

CREATE INDEX ix_customer_delinquency_open ON billing.customer_delinquency USING btree (merchant_id, customer_id, currency) WHERE (state <> 'current'::text);

ALTER TABLE ONLY billing.customer_delinquency
    ADD CONSTRAINT customer_delinquency_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE CASCADE;
ALTER TABLE ONLY billing.customer_delinquency
    ADD CONSTRAINT customer_delinquency_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

-- ---------------------------------------------------------------------------
-- PSPs and custodians
-- ---------------------------------------------------------------------------

CREATE TABLE billing.custodians (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    kind text NOT NULL,
    environment text DEFAULT 'live'::text NOT NULL,
    account_id text NOT NULL,
    settings jsonb DEFAULT '{}'::jsonb NOT NULL,
    credential_versions jsonb DEFAULT '{}'::jsonb NOT NULL,
    archived boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT custodians_environment_check CHECK ((environment = ANY (ARRAY['live'::text, 'test'::text]))),
    CONSTRAINT custodians_kind_check CHECK ((kind = ANY (ARRAY['basis_theory'::text, 'hyperswitch'::text]))),
    CONSTRAINT custodians_nonempty CHECK (((btrim(key) <> ''::text) AND (btrim(account_id) <> ''::text)))
);
COMMENT ON TABLE billing.custodians IS 'or#880: merchant custodian registry. A row is one merchant-owned account with a third-party card custodian (Basis Theory today). Custody is orthogonal to the rail: this says WHO HOLDS the card, openrails.psps says who charges it. Referenced by psps.custodian_id — one custodian can back many PSPs.';
COMMENT ON COLUMN billing.custodians.key IS 'The custodian''s manifest key (merchants.<slug>.custodians.<key>) — the name a PSP entry references.';
COMMENT ON COLUMN billing.custodians.kind IS 'The custodian VENDOR: basis_theory or hyperswitch. Same vocabulary as payment_methods.custodian, minus ''psp'' (which is the absence of a third-party custodian, not an account).';
COMMENT ON COLUMN billing.custodians.account_id IS 'The custodian-native tenant identity (Basis Theory: the tenant id). Operator-declared — there is no runtime whoami (#592).';
COMMENT ON COLUMN billing.custodians.settings IS 'Declared NON-secret knobs, validated against the kind''s registry (internal/custodians): public_api_key, network_tokens. Credentials are merchant secrets under custodians/<kind>/<environment>/<account_id>/<key>.';
COMMENT ON COLUMN billing.custodians.credential_versions IS 'or#812 rotation watermarks, per credential key: the Secret.Version each credential reached at its last rotation. A reader holding an older cached version must go back to the backend, so a rotation on one node is effective on every node the instant it commits. Absent/zero = no floor.';
COMMENT ON COLUMN billing.custodians.archived IS 'Drain-only lifecycle flag, matching psps.archived: true keeps the custodian addressable for instruments it already holds but excludes it from new arrangements.';

ALTER TABLE ONLY billing.custodians
    ADD CONSTRAINT custodians_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.custodians
    ADD CONSTRAINT uq_custodians_id_merchant UNIQUE (id, merchant_id);
ALTER TABLE ONLY billing.custodians
    ADD CONSTRAINT uq_custodians_merchant_id_kind UNIQUE (merchant_id, id, kind);

CREATE INDEX idx_custodians_merchant ON billing.custodians USING btree (merchant_id);
CREATE UNIQUE INDEX uq_custodians_identity ON billing.custodians USING btree (kind, environment, account_id);
CREATE UNIQUE INDEX uq_custodians_key ON billing.custodians USING btree (merchant_id, lower(key));

ALTER TABLE ONLY billing.custodians
    ADD CONSTRAINT custodians_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE;

CREATE TABLE billing.psps (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    environment text DEFAULT 'live'::text NOT NULL,
    account_id text NOT NULL,
    key text,
    evidence jsonb,
    first_seen_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    last_verified_at timestamp with time zone,
    replaced_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    archived boolean DEFAULT false NOT NULL,
    custodian_id uuid,
    pending_signer_public_key text,
    CONSTRAINT psps_environment_check CHECK ((environment = ANY (ARRAY['live'::text, 'test'::text]))),
    CONSTRAINT psps_nonempty CHECK (((btrim(rail) <> ''::text) AND (btrim(environment) <> ''::text) AND (btrim(account_id) <> ''::text)))
);
COMMENT ON TABLE billing.psps IS 'Merchant PSP registry. A row is one merchant-owned payment-service-provider account on one rail.';
COMMENT ON COLUMN billing.psps.rail IS 'Payment rail/backend such as stripe, nmi, ccbill, solana, or a future rail.';
COMMENT ON COLUMN billing.psps.environment IS 'Provider environment: live or test. Live and test accounts are distinct identities and may each have their own primary.';
COMMENT ON COLUMN billing.psps.account_id IS 'Provider-returned account identity, e.g. Stripe acct_..., NMI profile account id, CCBill account/subaccount, or Solana authority address.';
COMMENT ON COLUMN billing.psps.key IS 'The PSP''s manifest key (e.g. mobius) — the vocabulary catalog psp_links and checkout speak.';
COMMENT ON COLUMN billing.psps.archived IS 'Drain-only provider-account lifecycle flag. false means eligible for new work; true remains addressable for existing obligations and inbound provider events.';
COMMENT ON COLUMN billing.psps.custodian_id IS 'or#880: the custodian holding the instruments charged through this PSP. NULL = the PSP holds its own (Stripe pm_, NMI customer vault). Composite FK: a PSP can only reference ITS OWN merchant''s custodian.';

ALTER TABLE ONLY billing.psps
    ADD CONSTRAINT psps_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.psps
    ADD CONSTRAINT psps_merchant_id_id_key UNIQUE (merchant_id, id);
ALTER TABLE ONLY billing.psps
    ADD CONSTRAINT psps_merchant_id_id_rail_key UNIQUE (merchant_id, id, rail);

CREATE INDEX idx_psps_custodian ON billing.psps USING btree (custodian_id) WHERE (custodian_id IS NOT NULL);
CREATE INDEX idx_psps_merchant ON billing.psps USING btree (merchant_id);
CREATE INDEX idx_psps_new_work ON billing.psps USING btree (merchant_id, rail, environment, created_at DESC, id DESC) WHERE (archived = false);
CREATE UNIQUE INDEX uq_psps_identity ON billing.psps USING btree (rail, environment, account_id);

ALTER TABLE ONLY billing.psps
    ADD CONSTRAINT psps_custodian_fk FOREIGN KEY (custodian_id, merchant_id) REFERENCES billing.custodians(id, merchant_id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.psps
    ADD CONSTRAINT psps_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE;

CREATE TABLE billing.rail_customer_accounts (
    id uuid DEFAULT uuidv7() NOT NULL,
    rail text NOT NULL,
    account_id text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL
);
COMMENT ON TABLE billing.rail_customer_accounts IS 'customer <-> rail customer-id mapping, per PSP. Two accounts on one rail hold independent mappings (or#893 supersedes #704, which dropped psp_id when no writer set it).';
COMMENT ON COLUMN billing.rail_customer_accounts.psp_id IS 'PSP whose remote customer object this row maps. Required (or#893).';

ALTER TABLE ONLY billing.rail_customer_accounts
    ADD CONSTRAINT rail_customer_accounts_pkey PRIMARY KEY (id);

CREATE INDEX idx_rail_customer_accounts_customer ON billing.rail_customer_accounts USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE INDEX idx_rail_customer_accounts_merchant_id ON billing.rail_customer_accounts USING btree (merchant_id);
CREATE INDEX idx_rail_customer_accounts_psp ON billing.rail_customer_accounts USING btree (psp_id);
CREATE UNIQUE INDEX uq_rail_customer_accounts_customer_psp ON billing.rail_customer_accounts USING btree (merchant_id, customer_id, rail, psp_id);
CREATE UNIQUE INDEX uq_rail_customer_accounts_psp_account ON billing.rail_customer_accounts USING btree (merchant_id, rail, psp_id, account_id);

ALTER TABLE ONLY billing.rail_customer_accounts
    ADD CONSTRAINT rail_customer_accounts_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.rail_customer_accounts
    ADD CONSTRAINT rail_customer_accounts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.rail_customer_accounts
    ADD CONSTRAINT rail_customer_accounts_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id) ON DELETE CASCADE;

CREATE FUNCTION billing.psp_owner_by_identity(p_rail text, p_environment text, p_account_id text) RETURNS TABLE(id uuid, merchant_id uuid, rail text, environment text, account_id text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT p.id, p.merchant_id, p.rail, p.environment, p.account_id
      FROM billing.psps p
     WHERE p.rail = lower(p_rail)
       AND p.environment = p_environment
       AND p.account_id = p_account_id
     LIMIT 1;
END;
$$;
COMMENT ON FUNCTION billing.psp_owner_by_identity(p_rail text, p_environment text, p_account_id text) IS 'Cross-merchant PSP ownership lookup by the GLOBAL (rail, environment, account_id) natural key. The one sanctioned way to answer "which merchant owns this provider account" before a merchant context exists (inbound webhooks, the global-uniqueness preflight). Returns the routing tuple only — no credentials, no listing.';
REVOKE ALL ON FUNCTION billing.psp_owner_by_identity(p_rail text, p_environment text, p_account_id text) FROM PUBLIC;

CREATE FUNCTION billing.custodian_owner_by_identity(p_kind text, p_environment text, p_account_id text) RETURNS TABLE(id uuid, merchant_id uuid, key text, kind text, environment text, account_id text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT c.id, c.merchant_id, c.key, c.kind, c.environment, c.account_id
      FROM billing.custodians c
     WHERE c.kind = lower(p_kind)
       AND c.environment = p_environment
       AND c.account_id = p_account_id
     LIMIT 1;
END;
$$;
COMMENT ON FUNCTION billing.custodian_owner_by_identity(p_kind text, p_environment text, p_account_id text) IS 'or#880: cross-merchant custodian ownership lookup by the GLOBAL (kind, environment, account_id) natural key — the custody sibling of psp_owner_by_identity, for inbound custodian webhooks (Basis Theory) that carry a tenant id and no merchant context. A custodian may back several PSPs, so this deliberately resolves the CUSTODIAN, never "the" PSP.';
REVOKE ALL ON FUNCTION billing.custodian_owner_by_identity(p_kind text, p_environment text, p_account_id text) FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- Catalog
-- ---------------------------------------------------------------------------

-- Individual services acquire the merchant lock first. This trigger also
-- fences direct imports/module writes, keeping them visible to application CAS.
CREATE FUNCTION billing.catalog_authored_write() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE mid uuid;
BEGIN
    IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
    IF TG_OP='DELETE' THEN mid := OLD.merchant_id; ELSE mid := NEW.merchant_id; END IF;
    IF current_setting('app.catalog_batch',true) IS DISTINCT FROM mid::text THEN
        -- A legacy raw writer may already hold a child-row lock. Do not wait
        -- behind a merchant-first transaction while holding that child: refuse
        -- with a retryable serialization conflict instead of deadlocking.
        BEGIN
            PERFORM id FROM billing.merchants WHERE id=mid FOR UPDATE NOWAIT;
        EXCEPTION WHEN lock_not_available THEN
            RAISE EXCEPTION 'concurrent catalog authoring; retry the transaction' USING ERRCODE='40001';
        END;
        UPDATE billing.merchants SET catalog_revision=catalog_revision+1 WHERE id=mid;
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END $$;

CREATE FUNCTION billing.guard_catalog_identity() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP='DELETE' OR NEW.id IS DISTINCT FROM OLD.id
       OR NEW.merchant_id IS DISTINCT FROM OLD.merchant_id
       OR NEW.owner_subject IS DISTINCT FROM OLD.owner_subject THEN
        RAISE EXCEPTION 'catalog identity and ownership are immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

-- Catalog ownership is business data within one merchant; it grants no authority.
CREATE TABLE billing.catalogs (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    owner_subject text COLLATE "C",
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT catalogs_merchant_id_id_key UNIQUE (merchant_id, id),
    CONSTRAINT catalogs_owner_subject_nonempty CHECK (owner_subject IS NULL OR owner_subject <> '')
);
COMMENT ON TABLE billing.catalogs IS 'Immutable catalog identity within one merchant. NULL owner_subject is its default merchant catalog; non-NULL is an opaque verified host subject. Subject namespace must be preserved on authorized archive relocation.';

CREATE UNIQUE INDEX catalogs_one_default ON billing.catalogs (merchant_id) WHERE owner_subject IS NULL;
CREATE UNIQUE INDEX catalogs_one_owner ON billing.catalogs (merchant_id, owner_subject) WHERE owner_subject IS NOT NULL;

CREATE TRIGGER immutable_catalog_identity BEFORE UPDATE OR DELETE ON billing.catalogs
FOR EACH ROW EXECUTE FUNCTION billing.guard_catalog_identity();
CREATE TRIGGER catalog_authored_catalog BEFORE INSERT OR UPDATE OR DELETE ON billing.catalogs FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();

CREATE FUNCTION billing.ensure_default_catalog(p_merchant uuid) RETURNS uuid
LANGUAGE sql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
    INSERT INTO billing.catalogs (merchant_id)
    VALUES (p_merchant)
    ON CONFLICT (merchant_id) WHERE owner_subject IS NULL
    DO UPDATE SET updated_at=billing.catalogs.updated_at
    RETURNING id;
$$;
REVOKE ALL ON FUNCTION billing.ensure_default_catalog(uuid) FROM PUBLIC;

CREATE FUNCTION billing.assign_product_catalog() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.catalog_id IS NULL THEN
        NEW.catalog_id := billing.ensure_default_catalog(NEW.merchant_id);
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.guard_product_catalog_identity() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.merchant_id IS DISTINCT FROM OLD.merchant_id
       OR NEW.catalog_id IS DISTINCT FROM OLD.catalog_id THEN
        RAISE EXCEPTION 'product catalog identity is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

-- Product eligibility is a separate lifecycle flag. Its changes affect offers
-- without changing their own archive flags or repricing existing subscribers.
CREATE FUNCTION billing.catalog_product_offer_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.archived IS DISTINCT FROM OLD.archived THEN
  INSERT INTO billing.price_key_movements(merchant_id,key,price_id,effective_at,archived)
   SELECT p.merchant_id,p.key,p.id,clock_timestamp(),NEW.archived FROM billing.prices p
   WHERE p.merchant_id=NEW.merchant_id AND p.product_id=NEW.id AND NOT p.archived;
 END IF;
 RETURN NEW;
END $$;

-- Product identity cannot change underneath live billing or paid access.
-- The product UPDATE owns the row lock; subscription inserts/reactivations take
-- FOR SHARE on that same row before copying tier_group, serializing both orders.
CREATE FUNCTION billing.products_guard_tier_group() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.tier_group IS DISTINCT FROM OLD.tier_group AND EXISTS (
        SELECT 1 FROM billing.subscriptions s
        WHERE s.merchant_id = OLD.merchant_id AND s.product_id = OLD.id
          AND s.deleted_at IS NULL
          AND s.status IN ('active', 'pending', 'past_due', 'awaiting_method', 'unverified')
          AND (s.scheduled_price_id IS NOT NULL OR EXISTS (
                SELECT 1 FROM billing.rail_intents i
                WHERE i.merchant_id = s.merchant_id AND i.subscription_id = s.id
                  AND i.intent_type IN ('nmi_upgrade', 'stripe_tier_change', 'initial_membership')
                  AND i.status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable')))
    ) THEN
        RAISE EXCEPTION 'product tier group cannot change while a live subscription has a plan change in flight'
            USING ERRCODE = '23514', CONSTRAINT = 'products_live_subscription_tier_group';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.products_propagate_tier_group() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.tier_group IS DISTINCT FROM OLD.tier_group THEN
        UPDATE billing.subscriptions SET tier_group = NEW.tier_group
        WHERE merchant_id = NEW.merchant_id AND product_id = NEW.id AND deleted_at IS NULL;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TABLE billing.products (
    id uuid DEFAULT uuidv7() NOT NULL,
    key text NOT NULL,
    display_name text NOT NULL,
    description text,
    entitlements_spec jsonb,
    tier_group character varying(100),
    tier_rank integer DEFAULT 0 NOT NULL,
    archived boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    catalog_id uuid NOT NULL,
    CONSTRAINT products_catalog_present CHECK (catalog_id IS NOT NULL),
    CONSTRAINT products_catalog_fk FOREIGN KEY (merchant_id, catalog_id) REFERENCES billing.catalogs(merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT products_entitlement_hours_nonnegative CHECK (NOT jsonb_path_exists(coalesce(entitlements_spec, '{}'::jsonb), '$.* ? (@.type() == "number" && @ < 0)'))
);
COMMENT ON TABLE billing.products IS 'Product definitions that can be purchased or subscribed to';
COMMENT ON COLUMN billing.products.tier_group IS 'Semantic group name for mutually-exclusive products (e.g., "premium"). Products in same group require upgrade/downgrade, not parallel ownership.';
COMMENT ON COLUMN billing.products.tier_rank IS 'Tier ranking within group. Higher = more premium. Used to determine upgrade (higher rank) vs downgrade (lower rank) direction.';

ALTER TABLE ONLY billing.products
    ADD CONSTRAINT products_merchant_key_key UNIQUE (merchant_id, key);
ALTER TABLE ONLY billing.products
    ADD CONSTRAINT products_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.products
    ADD CONSTRAINT products_merchant_id_id_key UNIQUE (merchant_id, id);

CREATE INDEX idx_products_archived ON billing.products USING btree (archived);
CREATE INDEX idx_products_key ON billing.products USING btree (key);
CREATE INDEX idx_products_merchant_id ON billing.products USING btree (merchant_id);
CREATE INDEX idx_products_tier_group ON billing.products USING btree (tier_group) WHERE (tier_group IS NOT NULL);
CREATE INDEX products_catalog_id ON billing.products(merchant_id,catalog_id);
CREATE INDEX products_active_entitlements_spec
ON billing.products USING gin (entitlements_spec)
WHERE NOT archived;

ALTER TABLE ONLY billing.products
    ADD CONSTRAINT products_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TRIGGER assign_product_catalog BEFORE INSERT ON billing.products
FOR EACH ROW EXECUTE FUNCTION billing.assign_product_catalog();
CREATE TRIGGER immutable_product_catalog_identity BEFORE UPDATE ON billing.products
FOR EACH ROW EXECUTE FUNCTION billing.guard_product_catalog_identity();
CREATE TRIGGER trg_products_guard_tier_group BEFORE UPDATE OF tier_group ON billing.products
    FOR EACH ROW EXECUTE FUNCTION billing.products_guard_tier_group();
CREATE TRIGGER catalog_authored_product BEFORE INSERT OR UPDATE OR DELETE ON billing.products FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();
CREATE TRIGGER catalog_product_offer_state AFTER UPDATE OF archived ON billing.products FOR EACH ROW EXECUTE FUNCTION billing.catalog_product_offer_state();
CREATE TRIGGER trg_products_propagate_tier_group AFTER UPDATE OF tier_group ON billing.products
    FOR EACH ROW EXECUTE FUNCTION billing.products_propagate_tier_group();

CREATE FUNCTION billing.guard_product_archive_operation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'product archive operations are immutable' USING ERRCODE='23514';
END $$;

CREATE TABLE billing.product_archive_operations (
    merchant_id uuid NOT NULL REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256) = 32),
    product_id uuid NOT NULL,
    purchase_action text NOT NULL CHECK (purchase_action IN ('none','refund','review')),
    purchased_since timestamptz,
    reason text NOT NULL DEFAULT '' CHECK (length(reason) <= 500),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    UNIQUE (merchant_id, idempotency_key),
    FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products(merchant_id, id) ON DELETE RESTRICT,
    CHECK ((purchase_action = 'none') = (purchased_since IS NULL))
);
COMMENT ON TABLE billing.product_archive_operations IS '#1058: immutable product archive receipts; the resolved purchase window and action are fixed at acceptance.';

CREATE TRIGGER immutable_product_archive_operation BEFORE UPDATE OR DELETE ON billing.product_archive_operations
 FOR EACH ROW EXECUTE FUNCTION billing.guard_product_archive_operation();

CREATE FUNCTION billing.prices_default_key() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    product_key text;
BEGIN
    IF NEW.key IS NOT NULL AND btrim(NEW.key) <> '' THEN
        RETURN NEW;
    END IF;
    SELECT key INTO product_key FROM billing.products WHERE id = NEW.product_id AND merchant_id = NEW.merchant_id;
    IF product_key IS NULL THEN
        RAISE EXCEPTION 'prices_default_key: product % not found for price %', NEW.product_id, NEW.id;
    END IF;
    NEW.key := product_key || '-' || billing.price_interval_label(NEW.access_duration_hours, NEW.auto_renew);
    RETURN NEW;
END;
$$;

-- An unassigned pointer is a real historical state, not the previous active
-- offer. UUIDv7 movement IDs give deterministic tie ordering within a timestamp.
CREATE FUNCTION billing.catalog_price_retired() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT OLD.archived AND (NEW.archived OR NEW.key <> OLD.key) THEN
  INSERT INTO billing.price_key_movements(merchant_id,key,price_id,effective_at,archived) VALUES(OLD.merchant_id,OLD.key,OLD.id,clock_timestamp(),true);
 END IF;
 RETURN NEW;
END $$;

CREATE TABLE billing.prices (
    id uuid DEFAULT uuidv7() NOT NULL,
    product_id uuid NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
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
    CONSTRAINT prices_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text))),
    CONSTRAINT prices_trial_amount_nonneg_chk CHECK (((trial_unit_amount IS NULL) OR (trial_unit_amount >= 0))),
    CONSTRAINT prices_trial_both_or_neither_chk CHECK (((trial_unit_amount IS NULL) = (trial_duration_hours IS NULL))),
    CONSTRAINT prices_trial_needs_auto_renew_chk CHECK (((trial_unit_amount IS NULL) OR auto_renew)),
    CONSTRAINT prices_trial_period_positive_chk CHECK (((trial_duration_hours IS NULL) OR (trial_duration_hours > 0)))
);
COMMENT ON TABLE billing.prices IS 'Pricing tiers for products with rail-specific identifiers';
COMMENT ON COLUMN billing.prices.amount IS 'Price amount in row currency micros (1 major unit = 1,000,000).';
COMMENT ON COLUMN billing.prices.access_duration_hours IS 'access window in HOURS a purchase grants; NULL = indefinite/durable. For auto_renew, hours/24 is the provider billing cadence in days.';
COMMENT ON COLUMN billing.prices.auto_renew IS '#622 whether the price recharges and extends the window after access_duration_hours (recurring).';
COMMENT ON COLUMN billing.prices.trial_unit_amount IS '#622 optional first-phase price (micros); 0 = free trial; NULL = no trial.';
COMMENT ON COLUMN billing.prices.trial_duration_hours IS 'optional trial first-phase length in HOURS; NULL = no trial.';
COMMENT ON COLUMN billing.prices.key IS '#774: durable per-merchant-unique handle for this price''s substance-version chain. Immutable identity-wise (the row''s id is still the #662 substance UUID) but the LABEL can be relabeled in place (a key rename). At most one non-archived row per (merchant_id, key) — see uq_prices_merchant_key_current. Archived rows keep their key as a back-reference to the chain.';

ALTER TABLE ONLY billing.prices
    ADD CONSTRAINT prices_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.prices
    ADD CONSTRAINT prices_merchant_id_id_key UNIQUE (merchant_id, id);
ALTER TABLE ONLY billing.prices
    ADD CONSTRAINT unique_prices_product_amount_window UNIQUE NULLS NOT DISTINCT (product_id, amount, currency, access_duration_hours, auto_renew, trial_unit_amount, trial_duration_hours);

CREATE INDEX idx_prices_archived ON billing.prices USING btree (archived);
CREATE INDEX idx_prices_merchant_id ON billing.prices USING btree (merchant_id);
CREATE INDEX idx_prices_merchant_key ON billing.prices USING btree (merchant_id, key);
CREATE INDEX idx_prices_product_id ON billing.prices USING btree (product_id);
CREATE UNIQUE INDEX uq_prices_id_product_merchant ON billing.prices USING btree (id, product_id, merchant_id);
CREATE UNIQUE INDEX uq_prices_merchant_key_current ON billing.prices USING btree (merchant_id, key) WHERE (NOT archived);

ALTER TABLE ONLY billing.prices
    ADD CONSTRAINT prices_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.prices
    ADD CONSTRAINT prices_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products(merchant_id, id) ON DELETE RESTRICT;

CREATE TRIGGER trg_prices_default_key BEFORE INSERT ON billing.prices FOR EACH ROW EXECUTE FUNCTION billing.prices_default_key();
CREATE TRIGGER catalog_authored_price BEFORE INSERT OR UPDATE OR DELETE ON billing.prices FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();
CREATE TRIGGER catalog_price_retired AFTER UPDATE OF archived,key ON billing.prices FOR EACH ROW EXECUTE FUNCTION billing.catalog_price_retired();

CREATE TABLE billing.price_key_movements (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    price_id uuid NOT NULL,
    effective_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    archived boolean NOT NULL DEFAULT false
);
COMMENT ON TABLE billing.price_key_movements IS '#774: append-only log of when a price key''s current pointer moved to which price row. History, not row identity — a row can appear more than once (reactivation).';

ALTER TABLE ONLY billing.price_key_movements
    ADD CONSTRAINT price_key_movements_pkey PRIMARY KEY (id);

CREATE INDEX idx_price_key_movements_key ON billing.price_key_movements USING btree (merchant_id, key, effective_at DESC);
CREATE INDEX idx_price_key_movements_price ON billing.price_key_movements USING btree (price_id);

ALTER TABLE ONLY billing.price_key_movements
    ADD CONSTRAINT price_key_movements_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.price_key_movements
    ADD CONSTRAINT price_key_movements_price_fk FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices(merchant_id, id) ON DELETE RESTRICT;

-- Account identity owns provider price objects. Labels live only on psps.
CREATE TABLE billing.price_psp_bindings (
    merchant_id uuid NOT NULL,
    price_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    plan_id text,
    price_ref text,
    recurring_billing_option_id text,
    plan_pda text,
    flex_id text,
    configuration jsonb NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (merchant_id, price_id, psp_id),
    CONSTRAINT price_psp_bindings_price_fk FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices(merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT price_psp_bindings_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT price_psp_bindings_configuration_object CHECK (jsonb_typeof(configuration) = 'object'),
    CONSTRAINT price_psp_bindings_configuration_identity CHECK (NOT configuration ?| ARRAY['psp_id', 'rail', 'plan_id', 'price_id', 'recurring_billing_option_id', 'plan_pda', 'flex_id']),
    CONSTRAINT price_psp_bindings_plan_ref_nonempty CHECK (plan_id IS NULL OR btrim(plan_id) <> ''),
    CONSTRAINT price_psp_bindings_price_ref_nonempty CHECK (price_ref IS NULL OR btrim(price_ref) <> '')
);

CREATE UNIQUE INDEX uq_price_psp_bindings_plan ON billing.price_psp_bindings (merchant_id, psp_id, plan_id) WHERE plan_id IS NOT NULL;
CREATE UNIQUE INDEX uq_price_psp_bindings_price ON billing.price_psp_bindings (merchant_id, psp_id, price_ref) WHERE price_ref IS NOT NULL;
CREATE UNIQUE INDEX uq_price_psp_bindings_rbo ON billing.price_psp_bindings (merchant_id, psp_id, recurring_billing_option_id) WHERE recurring_billing_option_id IS NOT NULL;
CREATE UNIQUE INDEX uq_price_psp_bindings_pda ON billing.price_psp_bindings (merchant_id, psp_id, plan_pda) WHERE plan_pda IS NOT NULL;

CREATE TRIGGER catalog_authored_binding BEFORE INSERT OR UPDATE OR DELETE ON billing.price_psp_bindings FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();

CREATE TABLE billing.catalog_meters (
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    event_type text,
    value_property text,
    aggregation text,
    unit text,
    group_by jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT catalog_meters_aggregation_check CHECK (((aggregation IS NULL) OR (aggregation = ANY (ARRAY['sum'::text, 'count'::text, 'max'::text, 'min'::text, 'unique_count'::text, 'latest'::text])))),
    CONSTRAINT catalog_meters_key_nonempty CHECK ((btrim(key) <> ''::text))
);
COMMENT ON TABLE billing.catalog_meters IS '#599 billing meter registry. Meters are billed-later usage streams, distinct from #594 usage limits.';
COMMENT ON COLUMN billing.catalog_meters.event_type IS '#638 usage event type for rate-card meters; defaults to key when omitted.';
COMMENT ON COLUMN billing.catalog_meters.value_property IS '#638 JSON/dimension property carrying the numeric quantity to aggregate.';
COMMENT ON COLUMN billing.catalog_meters.aggregation IS '#638 aggregation mode for rate-card meters.';
COMMENT ON COLUMN billing.catalog_meters.group_by IS '#638 dimension name -> event metadata/dimension property mapping for matrix pricing.';

ALTER TABLE ONLY billing.catalog_meters
    ADD CONSTRAINT catalog_meters_pkey PRIMARY KEY (merchant_id, key);

ALTER TABLE ONLY billing.catalog_meters
    ADD CONSTRAINT catalog_meters_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TRIGGER catalog_authored_meter BEFORE INSERT OR UPDATE OR DELETE ON billing.catalog_meters FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();

CREATE TABLE billing.catalog_rate_cards (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    product_id uuid,
    ordinal integer NOT NULL,
    meter_key text,
    payment_term text DEFAULT 'in_arrears'::text NOT NULL,
    filter jsonb DEFAULT '{}'::jsonb NOT NULL,
    allowance jsonb,
    price jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    customer_id uuid,
    CONSTRAINT catalog_rate_cards_ordinal_positive CHECK ((ordinal >= 1)),
    CONSTRAINT catalog_rate_cards_payment_term_check CHECK ((payment_term = ANY (ARRAY['in_advance'::text, 'in_arrears'::text]))),
    CONSTRAINT catalog_rate_cards_product_scope_chk CHECK (((customer_id IS NOT NULL) OR (product_id IS NOT NULL)))
);
COMMENT ON TABLE billing.catalog_rate_cards IS '#638 rate-card sidecars: product usage/flat prices expressed as shared charge-model JSON. The ONLY metered-pricing engine (#707): legacy manifest metered: price declarations are translated into rate-card rows at push time (catalog_price_metered is gone).';
COMMENT ON COLUMN billing.catalog_rate_cards.customer_id IS '#798 negotiated per-payer override: when set, this card replaces the merchant-default card for the same meter_key when rating that payer.';

ALTER TABLE ONLY billing.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_pkey PRIMARY KEY (id);

CREATE UNIQUE INDEX uq_catalog_rate_cards_meter ON billing.catalog_rate_cards USING btree (merchant_id, meter_key) WHERE ((meter_key IS NOT NULL) AND (customer_id IS NULL));
CREATE UNIQUE INDEX uq_catalog_rate_cards_payer_meter ON billing.catalog_rate_cards USING btree (merchant_id, customer_id, meter_key) WHERE ((meter_key IS NOT NULL) AND (customer_id IS NOT NULL));
CREATE UNIQUE INDEX uq_catalog_rate_cards_product_ordinal ON billing.catalog_rate_cards USING btree (merchant_id, product_id, ordinal);

ALTER TABLE ONLY billing.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE CASCADE;
ALTER TABLE ONLY billing.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_meter_fk FOREIGN KEY (merchant_id, meter_key) REFERENCES billing.catalog_meters(merchant_id, key) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_product_fk FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products(merchant_id, id) ON DELETE CASCADE;

CREATE TRIGGER catalog_authored_rate_card BEFORE INSERT OR UPDATE OR DELETE ON billing.catalog_rate_cards FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();

-- Durable catalog application receipts and authored-write history.
CREATE TABLE billing.catalog_applications (
    merchant_id uuid NOT NULL REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    application_id text NOT NULL CHECK (length(application_id) BETWEEN 1 AND 128),
    catalog_id uuid NOT NULL,
    schema_version bigint NOT NULL,
    request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256)=32),
    base_revision bigint NOT NULL CHECK (base_revision >= 0),
    applied_revision bigint NOT NULL CHECK (applied_revision = base_revision + 1),
    result jsonb NOT NULL CHECK (octet_length(result::text) <= 16384),
    applied_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id,application_id),
    FOREIGN KEY (merchant_id,catalog_id) REFERENCES billing.catalogs(merchant_id,id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.catalog_applications IS 'Permanent compact replay receipts, retained and restored with the merchant billing book; never expire by HTTP idempotency TTL.';

CREATE TRIGGER immutable_catalog_application_receipt BEFORE UPDATE OR DELETE ON billing.catalog_applications FOR EACH ROW EXECUTE FUNCTION billing.guard_catalog_application_receipt();

-- ---------------------------------------------------------------------------
-- Payment methods
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.payment_method_default_candidate(p_merchant uuid, p_customer uuid) RETURNS uuid
    LANGUAGE sql STABLE
    AS $$
    SELECT pm.id FROM billing.payment_methods pm
    LEFT JOIN LATERAL (
        SELECT max(p.purchased_at) AS used_at
        FROM billing.subscriptions s
        JOIN billing.payments p ON p.merchant_id = s.merchant_id AND p.subscription_id = s.id
            AND p.deleted_at IS NULL AND p.status = 'completed'
        WHERE s.merchant_id = pm.merchant_id AND s.payment_method_id = pm.id
    ) used ON true
    WHERE pm.merchant_id = p_merchant AND pm.customer_id = p_customer AND pm.park_reason = ''
    ORDER BY used.used_at DESC NULLS LAST,
        (CASE WHEN regexp_replace(coalesce(pm.expiry_date, ''), '\D', '', 'g') !~ '^(0[1-9]|1[0-2])[0-9]{2}$' THEN 0
              WHEN make_date(2000 + substr(regexp_replace(pm.expiry_date, '\D', '', 'g'), 3, 2)::int,
                             substr(regexp_replace(pm.expiry_date, '\D', '', 'g'), 1, 2)::int, 1) + interval '1 month' <= now() THEN 1
              ELSE 0 END),
        pm.created_at DESC, pm.id DESC
    LIMIT 1
$$;

-- A parked method is never default.
CREATE FUNCTION billing.payment_methods_default_usable() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.park_reason <> '' THEN
        NEW.is_default := false;
    END IF;
    RETURN NEW;
END;
$$;

-- At commit, a customer whose methods changed keeps exactly one default. The
-- customer lock serializes concurrent commits, so two first saves end with one.
CREATE FUNCTION billing.payment_methods_ensure_default() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    m uuid;
    c uuid;
    pick uuid;
BEGIN
    FOR m, c IN
        SELECT DISTINCT x.merchant_id, x.customer_id FROM (
            SELECT NEW.merchant_id AS merchant_id, NEW.customer_id AS customer_id WHERE TG_OP <> 'DELETE'
            UNION ALL
            SELECT OLD.merchant_id, OLD.customer_id WHERE TG_OP <> 'INSERT'
        ) x
    LOOP
        PERFORM pg_advisory_xact_lock(hashtextextended('openrails.default_payment_method:' || m::text || ':' || c::text, 0));
        IF NOT EXISTS (SELECT 1 FROM billing.payment_methods WHERE merchant_id = m AND customer_id = c AND is_default) THEN
            pick := billing.payment_method_default_candidate(m, c);
            IF pick IS NOT NULL THEN
                UPDATE billing.payment_methods SET is_default = true WHERE merchant_id = m AND id = pick;
            END IF;
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

CREATE FUNCTION billing.guard_provider_cutover_card() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'UPDATE' AND (to_jsonb(NEW) - 'is_default' - 'updated_at') = (to_jsonb(OLD) - 'is_default' - 'updated_at') THEN
   RETURN NEW;
 END IF;
 IF EXISTS(SELECT 1 FROM billing.rail_intents i
   WHERE i.merchant_id=OLD.merchant_id AND i.intent_type='nmi_provider_cutover'
   AND i.status NOT IN ('succeeded','failed_terminal','superseded','expired')
   AND (i.payload->'request'->>'target_payment_method_id'=OLD.id::text
        OR i.payload->>'source_payment_method_id'=OLD.id::text)) THEN
   RAISE EXCEPTION 'payment method has an unresolved provider cutover' USING ERRCODE='55000';
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;

CREATE TABLE billing.payment_methods (
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
    psp_id uuid NOT NULL,
    rail_customer_ref text DEFAULT ''::text NOT NULL,
    rail_method_ref text DEFAULT ''::text NOT NULL,
    stored_credential_recurring_ref text DEFAULT ''::text NOT NULL,
    stored_credential_unscheduled_ref text DEFAULT ''::text NOT NULL,
    custodian text DEFAULT 'psp'::text NOT NULL,
    custodian_id uuid,
    CONSTRAINT payment_methods_custodian_identity CHECK ((custodian = 'psp') = (custodian_id IS NULL)),
    fingerprint text DEFAULT ''::text NOT NULL,
    network_token_id text DEFAULT ''::text NOT NULL,
    network_token_status text DEFAULT ''::text NOT NULL,
    network_token_par text DEFAULT ''::text NOT NULL,
    charge_via text DEFAULT 'pan_proxy'::text NOT NULL,
    park_reason text DEFAULT ''::text NOT NULL,
    parked_at timestamp with time zone,
    account_updater_checked_at timestamp with time zone,
    is_default boolean DEFAULT false NOT NULL,
    CONSTRAINT payment_methods_charge_via_check CHECK ((charge_via = ANY (ARRAY['pan_proxy'::text, 'network_token'::text]))),
    CONSTRAINT payment_methods_custodian_check CHECK ((custodian = ANY (ARRAY['psp'::text, 'basis_theory'::text, 'hyperswitch'::text]))),
    CONSTRAINT payment_methods_default_usable CHECK (NOT is_default OR park_reason = '')
);
COMMENT ON TABLE billing.payment_methods IS 'Generalized payment method table supporting multiple rails.';
COMMENT ON COLUMN billing.payment_methods.rail IS 'Payment rail type: nmi, ccbill, stripe, etc.';
COMMENT ON COLUMN billing.payment_methods.psp_id IS 'PSP that vaulted this payment method. Required (or#893).';
COMMENT ON COLUMN billing.payment_methods.rail_customer_ref IS 'Customer-scope rail handle (e.g. NMI customer_vault_id); '''' when the customer scope lives in rail_customers (Stripe).';
COMMENT ON COLUMN billing.payment_methods.rail_method_ref IS 'Instrument-scope rail handle (e.g. NMI billing_id, Stripe pm_, Spreedly/HyperSwitch token).';
COMMENT ON COLUMN billing.payment_methods.stored_credential_recurring_ref IS 'Rail-scoped stored-credential replay reference for the RECURRING card-network agreement (NMI: gateway transactionid of the initial recurring CIT, replayed as initial_transaction_id on recurring MITs). Empty = not captured yet.';
COMMENT ON COLUMN billing.payment_methods.stored_credential_unscheduled_ref IS 'Rail-scoped stored-credential replay reference for the UNSCHEDULED card-network agreement (NMI: gateway transactionid of the initial unscheduled CIT, replayed as initial_transaction_id on unscheduled MITs). Empty = not captured yet.';
COMMENT ON COLUMN billing.payment_methods.custodian IS 'or#880 who HOLDS this instrument, orthogonal to who charges it (rail + psp_id): psp = stored at the processor itself (Stripe pm_, NMI customer vault) | basis_theory or hyperswitch = neutral third-party vault. Never empty — "no stored instrument" (CCBill, Solana) is the absence of a row, not a custodian value.';
COMMENT ON COLUMN billing.payment_methods.fingerprint IS 'Custodian-issued stable fingerprint of the underlying PAN (Basis Theory''s default fingerprint expression), for dedup/lookup. '''' = the custodian issues none.';
COMMENT ON COLUMN billing.payment_methods.network_token_id IS '#795 BT network-token uuid; '''' = not provisioned.';
COMMENT ON COLUMN billing.payment_methods.network_token_status IS '#795 NT lifecycle status: ''''|active|inactive|suspended|deleted (webhook-folded; never touches PAN-side expiry).';
COMMENT ON COLUMN billing.payment_methods.network_token_par IS '#795 payment account reference from NT provisioning.';
COMMENT ON COLUMN billing.payment_methods.charge_via IS '#795 per-instrument charge routing: pan_proxy (detokenized FPAN through the vault proxy) | network_token (DPAN; gated off on NMI gateways).';
COMMENT ON COLUMN billing.payment_methods.park_reason IS '#795 instrument park marker (cancellation-last-resort): non-empty = vault-side problem (token deleted/expired, closed account); charges fail loudly, operator notified, subscriptions NEVER terminally cancelled by this.';
COMMENT ON COLUMN billing.payment_methods.parked_at IS '#795 when the instrument was parked; NULL = not parked.';
COMMENT ON COLUMN billing.payment_methods.account_updater_checked_at IS 'or#795: when this instrument was last SUBMITTED to a batch account-updater cycle (not when it last changed). NULL = never. The staleness half of the due-work predicate: an instrument refreshed inside the lookahead window is not re-submitted, so one renewal cycle costs at most one network lookup per card.';
COMMENT ON COLUMN billing.payment_methods.is_default IS 'The customer''s default payment method: exactly one per (merchant, customer) with a usable method (#1084).';

ALTER TABLE ONLY billing.payment_methods
    ADD CONSTRAINT payment_methods_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.payment_methods
    ADD CONSTRAINT payment_methods_merchant_payer_id_key UNIQUE (merchant_id, customer_id, id);
ALTER TABLE ONLY billing.payment_methods
    ADD CONSTRAINT payment_methods_merchant_id_id_key UNIQUE (merchant_id, id);

CREATE INDEX idx_payment_methods_custodian_method_ref ON billing.payment_methods USING btree (merchant_id, custodian_id, rail_method_ref) WHERE (custodian <> 'psp'::text);
CREATE INDEX idx_payment_methods_custodian_network_token ON billing.payment_methods USING btree (merchant_id, custodian_id, network_token_id) WHERE ((custodian <> 'psp'::text) AND (network_token_id <> ''::text));
CREATE INDEX idx_payment_methods_customer ON billing.payment_methods USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE INDEX idx_payment_methods_merchant_id ON billing.payment_methods USING btree (merchant_id);
CREATE INDEX idx_payment_methods_method_ref ON billing.payment_methods USING btree (rail, rail_method_ref);
CREATE INDEX idx_payment_methods_psp ON billing.payment_methods USING btree (psp_id);
CREATE INDEX idx_payment_methods_rail ON billing.payment_methods USING btree (rail);
CREATE INDEX ix_payment_methods_account_updater_due ON billing.payment_methods USING btree (merchant_id, custodian, account_updater_checked_at NULLS FIRST) WHERE ((custodian <> 'psp'::text) AND (rail_method_ref <> ''::text));
CREATE INDEX payment_methods_fingerprint_idx ON billing.payment_methods USING btree (merchant_id, fingerprint) WHERE (fingerprint <> ''::text);
CREATE UNIQUE INDEX uq_payment_methods_psp_instrument ON billing.payment_methods USING btree (merchant_id, psp_id, custodian_id, rail_customer_ref, rail_method_ref) NULLS NOT DISTINCT;
CREATE UNIQUE INDEX uq_payment_methods_customer_default ON billing.payment_methods USING btree (merchant_id, customer_id) WHERE is_default;

ALTER TABLE ONLY billing.payment_methods
    ADD CONSTRAINT payment_methods_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.payment_methods
    ADD CONSTRAINT payment_methods_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.payment_methods
    ADD CONSTRAINT payment_methods_custodian_fk FOREIGN KEY (merchant_id, custodian_id, custodian) REFERENCES billing.custodians(merchant_id, id, kind);
ALTER TABLE ONLY billing.payment_methods
    ADD CONSTRAINT payment_methods_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id);

CREATE TRIGGER guard_provider_cutover_card BEFORE UPDATE OR DELETE ON billing.payment_methods
FOR EACH ROW EXECUTE FUNCTION billing.guard_provider_cutover_card();
CREATE TRIGGER trg_payment_methods_default_usable BEFORE INSERT OR UPDATE OF park_reason, is_default ON billing.payment_methods
    FOR EACH ROW EXECUTE FUNCTION billing.payment_methods_default_usable();
CREATE CONSTRAINT TRIGGER trg_payment_methods_ensure_default
    AFTER INSERT OR DELETE OR UPDATE OF is_default, park_reason, customer_id ON billing.payment_methods
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing.payment_methods_ensure_default();

CREATE TABLE billing.payment_method_updates (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    payment_method_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    source text NOT NULL,
    kind text NOT NULL,
    event_ref text NOT NULL,
    at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT payment_method_updates_pkey PRIMARY KEY (id),
    CONSTRAINT payment_method_updates_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE,
    CONSTRAINT payment_method_updates_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id),
    CONSTRAINT payment_method_updates_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id),
    CONSTRAINT chk_payment_method_updates_source CHECK (source IN ('nmi_acu', 'bt_account_updater', 'customer')),
    CONSTRAINT chk_payment_method_updates_kind CHECK (kind IN ('updated', 'closed_account', 'contact_customer')),
    CONSTRAINT chk_payment_method_updates_event_ref CHECK (event_ref <> '')
);
COMMENT ON TABLE billing.payment_method_updates IS '#1115 changes to a stored card''s standing, by source (nmi_acu, bt_account_updater, customer) and kind; event_ref makes a redelivered notice a no-op.';

CREATE UNIQUE INDEX uq_payment_method_updates_event ON billing.payment_method_updates USING btree (merchant_id, source, event_ref, payment_method_id);
CREATE INDEX idx_payment_method_updates_method ON billing.payment_method_updates USING btree (merchant_id, payment_method_id, at);

CREATE TABLE billing.custody_migrations (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    batch_id uuid NOT NULL,
    payment_method_id uuid NOT NULL,
    rail text NOT NULL,
    from_custodian text NOT NULL,
    from_custodian_id uuid,
    from_rail_customer_ref text DEFAULT ''::text NOT NULL,
    from_rail_method_ref text DEFAULT ''::text NOT NULL,
    from_psp_id uuid,
    to_custodian text NOT NULL,
    to_custodian_id uuid NOT NULL,
    to_rail_method_ref text NOT NULL,
    to_psp_id uuid,
    exported_at timestamp with time zone,
    outcome text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT chk_custody_migrations_outcome CHECK ((outcome = ANY (ARRAY['remapped'::text, 'created'::text]))),
    CONSTRAINT chk_custody_migrations_target CHECK (((btrim(to_rail_method_ref) <> ''::text) AND (btrim(to_custodian) <> ''::text)))
);
COMMENT ON TABLE billing.custody_migrations IS 'or#297 Phase C: one row per instrument whose CUSTODY changed — the durable memory of a vault-export remap. Records where the card used to live (the PSP vault handle the processor holds) and where it lives now (the custodian token), on an unchanged payment_method_id so subscriptions never move. Reversible in RECORD, never in custody: the fields to re-point an instrument back are all here, but a processor that deleted the vault entry or terminated the merchant cannot be undone by a row.';
COMMENT ON COLUMN billing.custody_migrations.batch_id IS 'The operator run that produced this row. A dry-run plan writes nothing; an applied run stamps every flip with one batch id so the report and the audit agree.';
COMMENT ON COLUMN billing.custody_migrations.from_rail_customer_ref IS 'The PSP-scope vault handle the instrument had BEFORE the flip (NMI customer_vault_id). Retained on the payment_methods row too — this is the copy that survives a later re-remap.';
COMMENT ON COLUMN billing.custody_migrations.from_rail_method_ref IS 'The instrument-scope handle before the flip (NMI billing_id; empty for the one-vault-per-card default, #682).';
COMMENT ON COLUMN billing.custody_migrations.to_rail_method_ref IS 'The custodian token id the instrument now charges through — payment_methods.rail_method_ref after the flip.';
COMMENT ON COLUMN billing.custody_migrations.exported_at IS 'The declared horizon of the custodian''s ingest of the vault export — when the token set was true.';
COMMENT ON COLUMN billing.custody_migrations.outcome IS 'remapped = an existing instrument changed custody (same payment_method_id, subscriptions untouched); created = the export carried a card with no local instrument and the operator declared its customer.';

ALTER TABLE ONLY billing.custody_migrations
    ADD CONSTRAINT custody_migrations_pkey PRIMARY KEY (id);

CREATE INDEX idx_custody_migrations_batch ON billing.custody_migrations USING btree (merchant_id, batch_id, created_at);
CREATE UNIQUE INDEX uq_custody_migrations_target ON billing.custody_migrations USING btree (merchant_id, payment_method_id, to_rail_method_ref);

ALTER TABLE ONLY billing.custody_migrations
    ADD CONSTRAINT custody_migrations_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE;
ALTER TABLE ONLY billing.custody_migrations
    ADD CONSTRAINT custody_migrations_payment_method_fk FOREIGN KEY (merchant_id, payment_method_id) REFERENCES billing.payment_methods(merchant_id, id) ON DELETE CASCADE;

-- ---------------------------------------------------------------------------
-- Subscriptions
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.subscriptions_set_tier_group() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    SELECT prod.tier_group INTO NEW.tier_group
    FROM billing.products AS prod
    WHERE prod.id = NEW.product_id AND prod.merchant_id = NEW.merchant_id
    FOR SHARE;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.subscriptions_record_status_transition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    decision text := nullif(current_setting('billing.decision', true), '');
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    IF TG_OP = 'INSERT' THEN
        INSERT INTO billing.subscription_status_transitions
            (merchant_id, subscription_id, from_status, to_status, cancel_type, occurred_at, decision, to_paid_through)
        VALUES (NEW.merchant_id, NEW.id, NULL, NEW.status, NEW.cancel_type, now(), 'created', NEW.current_period_ends_at);
    ELSIF OLD.status IS DISTINCT FROM NEW.status OR OLD.current_period_ends_at IS DISTINCT FROM NEW.current_period_ends_at THEN
        INSERT INTO billing.subscription_status_transitions
            (merchant_id, subscription_id, from_status, to_status, cancel_type, occurred_at, decision, from_paid_through, to_paid_through)
        VALUES (NEW.merchant_id, NEW.id, OLD.status, NEW.status, NEW.cancel_type, now(), decision, OLD.current_period_ends_at, NEW.current_period_ends_at);
    END IF;
    RETURN NEW;
END;
$$;

-- Deferred to commit so a row that enters and leaves unverified in one
-- transaction (an import resolving what it seeded) is neither tracked nor read.
CREATE FUNCTION billing.subscriptions_track_unverified() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    current_status billing.subscription_status;
    entered_at timestamp with time zone;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NULL; END IF;
    SELECT s.status, s.updated_at INTO current_status, entered_at
      FROM billing.subscriptions s
     WHERE s.merchant_id = NEW.merchant_id AND s.id = NEW.id AND s.deleted_at IS NULL;
    IF current_status = 'unverified' THEN
        INSERT INTO billing.subscription_verifications (merchant_id, subscription_id, since)
        VALUES (NEW.merchant_id, NEW.id, entered_at)
        ON CONFLICT (merchant_id, subscription_id) DO NOTHING;
        IF FOUND THEN
            PERFORM pg_notify('openrails_unverified:' || TG_TABLE_SCHEMA, NEW.merchant_id::text || ':' || NEW.id::text);
        END IF;
    ELSE
        DELETE FROM billing.subscription_verifications WHERE merchant_id = NEW.merchant_id AND subscription_id = NEW.id;
    END IF;
    RETURN NULL;
END;
$$;

CREATE FUNCTION billing.subscriptions_lifecycle_single_writer() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    changed text[] := '{}';
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN changed := changed || format('status %s->%s', OLD.status, NEW.status); END IF;
    IF NEW.current_period_starts_at IS DISTINCT FROM OLD.current_period_starts_at THEN changed := changed || 'current_period_starts_at'::text; END IF;
    IF NEW.current_period_ends_at IS DISTINCT FROM OLD.current_period_ends_at THEN changed := changed || 'current_period_ends_at'::text; END IF;
    IF NEW.cancel_type IS DISTINCT FROM OLD.cancel_type THEN changed := changed || 'cancel_type'::text; END IF;
    IF NEW.cancelled_at IS DISTINCT FROM OLD.cancelled_at THEN changed := changed || 'cancelled_at'::text; END IF;
    IF NEW.ended_at IS DISTINCT FROM OLD.ended_at THEN changed := changed || 'ended_at'::text; END IF;
    IF NEW.lifecycle_rev IS DISTINCT FROM OLD.lifecycle_rev AND NEW.lifecycle_rev <> OLD.lifecycle_rev + 1 THEN
        RAISE EXCEPTION 'subscription % lifecycle_rev must advance by one (% -> %)', OLD.id, OLD.lifecycle_rev, NEW.lifecycle_rev
            USING ERRCODE = '55000', CONSTRAINT = 'subscriptions_lifecycle_single_writer';
    END IF;
    IF cardinality(changed) > 0 AND NEW.lifecycle_rev = OLD.lifecycle_rev THEN
        RAISE EXCEPTION 'subscription % lifecycle fields changed outside a lifecycle decision: %', OLD.id, array_to_string(changed, ', ')
            USING ERRCODE = '55000', CONSTRAINT = 'subscriptions_lifecycle_single_writer';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.subscriptions_row_version() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.preserve_subscription_collection_policy() RETURNS trigger
 LANGUAGE plpgsql SET search_path TO 'billing','pg_catalog' AS $$
BEGIN
 IF NEW.collection_policy IS DISTINCT FROM OLD.collection_policy THEN
  RAISE EXCEPTION 'subscription collection policy is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;

-- An unresolved account cutover owns the subscription and both cards.
-- These fences cover all local writers, including lifecycle and custody remap,
-- while provider steps commit their receipts in independent transactions.
CREATE FUNCTION billing.guard_provider_cutover_subscription() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE op record;
BEGIN
 FOR op IN SELECT * FROM billing.rail_intents
   WHERE merchant_id=OLD.merchant_id AND subscription_id=OLD.id
     AND intent_type='nmi_provider_cutover'
     AND status NOT IN ('succeeded','failed_terminal','superseded','expired')
 LOOP
   IF TG_OP='UPDATE'
      AND op.result_evidence->>'source_canceled'='true'
      AND op.result_evidence->>'target_active'='true'
      AND NEW.psp_id=(op.payload->'request'->>'expected_target_psp_id')::uuid
      AND NEW.payment_method_id=(op.payload->'request'->>'target_payment_method_id')::uuid
      AND NEW.rail_subscription_id=op.result_evidence->'target'->>'id'
      AND (to_jsonb(NEW)-ARRAY['psp_id','payment_method_id','rail_subscription_id','updated_at'])
         =(to_jsonb(OLD)-ARRAY['psp_id','payment_method_id','rail_subscription_id','updated_at'])
   THEN RETURN NEW; END IF;
   RAISE EXCEPTION 'subscription has an unresolved provider cutover' USING ERRCODE='55000';
 END LOOP;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;

CREATE TABLE billing.subscriptions (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid,
    product_id uuid NOT NULL,
    status billing.subscription_status DEFAULT 'pending'::billing.subscription_status NOT NULL,
    rail text NOT NULL,
    collection_policy text DEFAULT 'provider' NOT NULL,
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
    gateway_response jsonb,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    tier_group character varying(100),
    deletion_scheduled_at timestamp with time zone,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    deleted_at timestamp with time zone,
    destructive_run_id uuid,
    destructive_run_class text GENERATED ALWAYS AS (CASE WHEN destructive_run_id IS NOT NULL THEN 'destructive' END) STORED,
    transient_retries integer DEFAULT 0 NOT NULL,
    lifecycle_rev bigint DEFAULT 0 NOT NULL,
    row_version bigint DEFAULT 0 NOT NULL,
    dunning_policy jsonb,
    CONSTRAINT subscriptions_engine_binding_check CHECK (collection_policy <> 'engine' OR ((rail IN ('nmi','stripe') AND rail_subscription_id='') OR rail='solana')),
    CONSTRAINT chk_cancelled_has_timestamp CHECK (((status <> 'cancelled'::billing.subscription_status) OR (cancelled_at IS NOT NULL))),
    CONSTRAINT chk_cancelled_has_type CHECK (((status <> 'cancelled'::billing.subscription_status) OR (cancel_type IS NOT NULL))),
    CONSTRAINT chk_cancelled_no_retry_schedule CHECK (((status <> 'cancelled'::billing.subscription_status) OR ((next_retry_at IS NULL) AND (grace_ends_at IS NULL)))),
    CONSTRAINT chk_ended_not_before_cancelled CHECK (((ended_at IS NULL) OR (cancelled_at IS NULL) OR (ended_at >= cancelled_at))),
    CONSTRAINT chk_past_due_has_period_end CHECK (((status <> 'past_due'::billing.subscription_status) OR (current_period_ends_at IS NOT NULL))),
    CONSTRAINT chk_valid_period CHECK (((current_period_starts_at IS NULL) OR (current_period_ends_at IS NULL) OR (current_period_starts_at < current_period_ends_at))),
    CONSTRAINT chk_subscriptions_transient_retries CHECK (transient_retries >= 0),
    CONSTRAINT subscriptions_collection_policy_check CHECK (collection_policy IN ('provider', 'nmi_schedule', 'engine')),
    CONSTRAINT subscriptions_nmi_schedule_rail_check CHECK ((collection_policy = 'nmi_schedule') = (rail = 'nmi' AND collection_policy <> 'engine'))
);
COMMENT ON TABLE billing.subscriptions IS 'Core subscription records tracking user billing relationships';
COMMENT ON COLUMN billing.subscriptions.product_id IS 'Denormalized product ID for efficient user+product lookups without joining prices';
COMMENT ON COLUMN billing.subscriptions.scheduled_price_id IS 'Price ID for scheduled tier change (downgrade). Applied at end of current billing period during renewal.';
COMMENT ON COLUMN billing.subscriptions.tier_group IS 'Denormalized from openrails.products.tier_group (kept in sync by trigger trg_subscriptions_set_tier_group). Backs uq_subscriptions_customer_tier_group_active, which enforces one active/pending/past_due/unknown subscription per (customer, tier group). Regrouping is prohibited while the product has live subscriptions.';
COMMENT ON COLUMN billing.subscriptions.psp_id IS 'PSP that produced this remote subscription mirror row. Required (or#893).';
COMMENT ON COLUMN billing.subscriptions.deleted_at IS 'or#858 soft delete: set, the row is invisible to every live read. Only `pull-provider --prune` sets it, and `openrails undo-run` clears it.';

ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_merchant_payer_id_key UNIQUE (merchant_id, customer_id, id);
ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_merchant_id_id_key UNIQUE (merchant_id, id);

CREATE INDEX idx_subscriptions_customer ON billing.subscriptions USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE INDEX idx_subscriptions_customer_active_created ON billing.subscriptions USING btree (customer_id, created_at DESC) WHERE (status = 'active'::billing.subscription_status);
CREATE INDEX idx_subscriptions_destructive_run ON billing.subscriptions USING btree (destructive_run_id) WHERE (destructive_run_id IS NOT NULL);
CREATE INDEX idx_subscriptions_engine_due ON billing.subscriptions (merchant_id, current_period_ends_at, next_retry_at) WHERE collection_policy = 'engine' AND status IN ('active', 'past_due') AND deleted_at IS NULL;
CREATE INDEX idx_subscriptions_due_dunning ON billing.subscriptions USING btree (next_retry_at, rail) WHERE ((status = 'past_due'::billing.subscription_status) AND (next_retry_at IS NOT NULL));
CREATE INDEX idx_subscriptions_gateway_order_id ON billing.subscriptions USING btree (merchant_id, rail, ((gateway_response ->> 'order_id'::text))) WHERE ((gateway_response ->> 'order_id'::text) IS NOT NULL);
CREATE INDEX idx_subscriptions_grace_ends_at ON billing.subscriptions USING btree (grace_ends_at) WHERE (grace_ends_at IS NOT NULL);
CREATE INDEX idx_subscriptions_merchant_cancelled ON billing.subscriptions USING btree (merchant_id, cancelled_at) WHERE (cancelled_at IS NOT NULL);
CREATE INDEX idx_subscriptions_merchant_ended ON billing.subscriptions USING btree (merchant_id, ended_at) WHERE (ended_at IS NOT NULL);
CREATE INDEX idx_subscriptions_merchant_id ON billing.subscriptions USING btree (merchant_id);
CREATE INDEX idx_subscriptions_merchant_started ON billing.subscriptions USING btree (merchant_id, started_at);
CREATE INDEX idx_subscriptions_next_retry_at ON billing.subscriptions USING btree (next_retry_at) WHERE (next_retry_at IS NOT NULL);
CREATE INDEX idx_subscriptions_payment_method_id ON billing.subscriptions USING btree (payment_method_id);
CREATE INDEX idx_subscriptions_period_overdue ON billing.subscriptions USING btree (current_period_ends_at) WHERE (status = 'active'::billing.subscription_status);
CREATE INDEX idx_subscriptions_price_id ON billing.subscriptions USING btree (price_id);
CREATE INDEX idx_subscriptions_product_id ON billing.subscriptions USING btree (product_id);
CREATE INDEX idx_subscriptions_psp ON billing.subscriptions USING btree (psp_id);
CREATE INDEX idx_subscriptions_rail ON billing.subscriptions USING btree (rail);
CREATE INDEX idx_subscriptions_rail_subscription ON billing.subscriptions USING btree (rail, rail_subscription_id);
CREATE INDEX idx_subscriptions_status ON billing.subscriptions USING btree (status);
CREATE UNIQUE INDEX uq_subscriptions_merchant_psp_subscription_id ON billing.subscriptions USING btree (merchant_id, rail, psp_id, rail_subscription_id) WHERE ((rail_subscription_id <> ''::text) AND (deleted_at IS NULL));
CREATE INDEX idx_subscriptions_engine_due_global ON billing.subscriptions (current_period_ends_at,merchant_id) WHERE collection_policy='engine' AND status IN ('active','past_due') AND deleted_at IS NULL;
CREATE UNIQUE INDEX uq_subscriptions_customer_product_lifecycle ON billing.subscriptions USING btree (merchant_id, customer_id, product_id)
    WHERE status IN ('active', 'pending', 'past_due', 'awaiting_method') AND deleted_at IS NULL;
CREATE UNIQUE INDEX uq_subscriptions_customer_tier_group_active ON billing.subscriptions USING btree (merchant_id, customer_id, tier_group)
    WHERE status IN ('active', 'pending', 'past_due', 'awaiting_method', 'unverified') AND tier_group IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX ix_subscriptions_renewal_by_payment_method ON billing.subscriptions USING btree (payment_method_id, current_period_ends_at)
    WHERE deleted_at IS NULL AND payment_method_id IS NOT NULL AND status IN ('active', 'past_due', 'awaiting_method');
CREATE INDEX idx_subscriptions_rebill_watch ON billing.subscriptions USING btree (merchant_id, current_period_ends_at)
    WHERE status IN ('active', 'unverified', 'awaiting_method') AND collection_policy IN ('engine', 'nmi_schedule') AND deleted_at IS NULL;

ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_destructive_run_fk FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs(merchant_id, id, run_class) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_payment_method_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_method_id) REFERENCES billing.payment_methods(merchant_id, customer_id, id) ON DELETE SET NULL (payment_method_id);
ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices(merchant_id, id);
ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_price_product_merchant_fkey FOREIGN KEY (price_id, product_id, merchant_id) REFERENCES billing.prices(id, product_id, merchant_id);
ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products(merchant_id, id);
ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id);
ALTER TABLE ONLY billing.subscriptions
    ADD CONSTRAINT subscriptions_scheduled_price_id_fkey FOREIGN KEY (merchant_id, scheduled_price_id) REFERENCES billing.prices(merchant_id, id);

CREATE TRIGGER trg_subscriptions_set_tier_group BEFORE INSERT OR UPDATE OF product_id, tier_group, status, deleted_at ON billing.subscriptions FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_set_tier_group();
CREATE TRIGGER guard_provider_cutover_subscription BEFORE UPDATE OR DELETE ON billing.subscriptions
FOR EACH ROW EXECUTE FUNCTION billing.guard_provider_cutover_subscription();
CREATE TRIGGER subscriptions_collection_policy_immutable BEFORE UPDATE OF collection_policy
 ON billing.subscriptions FOR EACH ROW EXECUTE FUNCTION billing.preserve_subscription_collection_policy();
CREATE CONSTRAINT TRIGGER trg_subscriptions_track_unverified AFTER INSERT OR UPDATE OF status ON billing.subscriptions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_track_unverified();
CREATE TRIGGER subscriptions_lifecycle_single_writer BEFORE UPDATE ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_lifecycle_single_writer();
CREATE TRIGGER subscriptions_row_version BEFORE UPDATE ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_row_version();
CREATE TRIGGER trg_subscriptions_status_transition AFTER INSERT OR UPDATE OF status, current_period_ends_at ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_record_status_transition();

CREATE TABLE billing.subscription_status_transitions (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    from_status billing.subscription_status,
    to_status billing.subscription_status NOT NULL,
    cancel_type text,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    decision text,
    from_paid_through timestamp with time zone,
    to_paid_through timestamp with time zone,
    CONSTRAINT chk_sst_real_transition CHECK (from_status IS DISTINCT FROM to_status OR from_paid_through IS DISTINCT FROM to_paid_through)
);
COMMENT ON TABLE billing.subscription_status_transitions IS '#733 append-only subscription status audit, written by trg_subscriptions_status_transition in the SAME tx as the status change. from_status NULL = row creation. Not retroactive: history begins at go-live.';
COMMENT ON COLUMN billing.subscription_status_transitions.cancel_type IS 'cancel_type on the subscription at transition time (meaningful for to_status=cancelled).';

ALTER TABLE ONLY billing.subscription_status_transitions
    ADD CONSTRAINT subscription_status_transitions_pkey PRIMARY KEY (id);

CREATE INDEX idx_sst_merchant_occurred ON billing.subscription_status_transitions USING btree (merchant_id, occurred_at);
CREATE INDEX idx_sst_subscription ON billing.subscription_status_transitions USING btree (subscription_id, occurred_at);

ALTER TABLE ONLY billing.subscription_status_transitions
    ADD CONSTRAINT sst_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.subscription_status_transitions
    ADD CONSTRAINT sst_subscription_fk FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, id) ON DELETE CASCADE;

CREATE TRIGGER immutable_subscription_status_transitions BEFORE UPDATE OR DELETE ON billing.subscription_status_transitions
FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.subscription_verifications (
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    since timestamp with time zone NOT NULL,
    reads integer DEFAULT 0 NOT NULL,
    last_read_at timestamp with time zone,
    last_error text,
    CONSTRAINT subscription_verifications_pkey PRIMARY KEY (merchant_id, subscription_id),
    CONSTRAINT subscription_verifications_subscription_fk FOREIGN KEY (merchant_id, subscription_id)
        REFERENCES billing.subscriptions(merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.subscription_verifications IS '#1094: one row per unverified subscription, kept by trg_subscriptions_track_unverified at commit. since dates entry (the row''s updated_at); reads/last_read_at record provider reads. Feeds life.unverified.backlog and the unresolved escalation.';

CREATE INDEX idx_subscription_verifications_since ON billing.subscription_verifications USING btree (merchant_id, since);

CREATE TABLE billing.reprice_batches (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    price_key text,
    to_price_id uuid NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    subscriptions_matched integer DEFAULT 0 NOT NULL,
    subscriptions_scheduled integer DEFAULT 0 NOT NULL,
    subscriptions_skipped integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    kind text DEFAULT 'reprice'::text NOT NULL,
    source_price_id uuid,
    fallback_policy text DEFAULT ''::text NOT NULL,
    subscriptions_blocked integer DEFAULT 0 NOT NULL,
    CONSTRAINT reprice_batches_fallback_chk CHECK ((fallback_policy = ANY (ARRAY[''::text, 'keep_grandfathered'::text, 'cancel_at_period_end'::text]))),
    CONSTRAINT reprice_batches_kind_chk CHECK ((kind = ANY (ARRAY['reprice'::text, 'plan_change'::text])))
);
COMMENT ON TABLE billing.reprice_batches IS '#773: header row for one bulk reprice operation (reprice_all_prior_versions or a single ad-hoc reprice); subscription_reprices rows carry reprice_batch_id back to it for per-subscription progress.';
COMMENT ON COLUMN billing.reprice_batches.source_price_id IS '#813: the retired plan''s price for a plan_change batch (the cohort selector); NULL for #773 price-key batches.';
COMMENT ON COLUMN billing.reprice_batches.fallback_policy IS '#813: operator''s choice for subscriptions on rails that cannot be auto-migrated (ccbill/solana): keep_grandfathered leaves them billing the archived source; cancel_at_period_end schedules their cancellation.';

ALTER TABLE ONLY billing.reprice_batches
    ADD CONSTRAINT reprice_batches_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.reprice_batches
    ADD CONSTRAINT reprice_batches_merchant_id_id_key UNIQUE (merchant_id, id);

CREATE INDEX idx_reprice_batches_merchant ON billing.reprice_batches USING btree (merchant_id, created_at DESC);
CREATE INDEX idx_reprice_batches_price_key ON billing.reprice_batches USING btree (merchant_id, price_key, created_at DESC) WHERE (price_key IS NOT NULL);

ALTER TABLE ONLY billing.reprice_batches
    ADD CONSTRAINT reprice_batches_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.reprice_batches
    ADD CONSTRAINT reprice_batches_source_price_fk FOREIGN KEY (merchant_id, source_price_id) REFERENCES billing.prices(merchant_id, id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.reprice_batches
    ADD CONSTRAINT reprice_batches_to_price_fk FOREIGN KEY (merchant_id, to_price_id) REFERENCES billing.prices(merchant_id, id) ON DELETE RESTRICT;

CREATE TABLE billing.subscription_reprices (
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
    kind text DEFAULT 'reprice'::text NOT NULL,
    blocked_reason text DEFAULT ''::text NOT NULL,
    CONSTRAINT subscription_reprices_applied_has_timestamp CHECK (((status <> 'applied'::text) OR (applied_at IS NOT NULL))),
    CONSTRAINT subscription_reprices_blocked_has_reason CHECK (((status <> 'blocked'::text) OR (blocked_reason <> ''::text))),
    CONSTRAINT subscription_reprices_canceled_has_timestamp CHECK (((status <> 'canceled'::text) OR (canceled_at IS NOT NULL))),
    CONSTRAINT subscription_reprices_kind_chk CHECK ((kind = ANY (ARRAY['reprice'::text, 'plan_change'::text]))),
    CONSTRAINT subscription_reprices_status_chk CHECK ((status = ANY (ARRAY['scheduled'::text, 'applied'::text, 'canceled'::text, 'blocked'::text])))
);
COMMENT ON TABLE billing.subscription_reprices IS '#773: a scheduled, applied, or canceled price move for one subscription. Applied at the subscription''s first renewal on/after effective_at (v1: no proration/mid-cycle).';
COMMENT ON COLUMN billing.subscription_reprices.acknowledged_short_notice IS '#781: true when this INCREASE reprice''s effective_at was inside the merchant''s configured notice window and was scheduled anyway via the explicit acknowledge_short_notice override on the request — the audit record for the support/emergency bypass path.';
COMMENT ON COLUMN billing.subscription_reprices.kind IS '#813: ''reprice'' = #773 same-product price move; ''plan_change'' = cross-product migration — the renewal-boundary pickup also moves product_id and cuts entitlement/credit snapshots over.';
COMMENT ON COLUMN billing.subscription_reprices.blocked_reason IS '#813: why this row could not be auto-scheduled (rail_requires_user_action, missing rail config, rail push failure). Only set when status=blocked.';

ALTER TABLE ONLY billing.subscription_reprices
    ADD CONSTRAINT subscription_reprices_pkey PRIMARY KEY (id);

CREATE INDEX idx_subscription_reprices_batch ON billing.subscription_reprices USING btree (reprice_batch_id) WHERE (reprice_batch_id IS NOT NULL);
CREATE INDEX idx_subscription_reprices_blocked_plan_change ON billing.subscription_reprices USING btree (merchant_id) WHERE ((status = 'blocked'::text) AND (kind = 'plan_change'::text));
CREATE INDEX idx_subscription_reprices_due ON billing.subscription_reprices USING btree (effective_at) WHERE (status = 'scheduled'::text);
CREATE INDEX idx_subscription_reprices_merchant ON billing.subscription_reprices USING btree (merchant_id, created_at DESC);
CREATE INDEX idx_subscription_reprices_subscription ON billing.subscription_reprices USING btree (subscription_id);
CREATE UNIQUE INDEX uq_subscription_reprices_one_scheduled ON billing.subscription_reprices USING btree (subscription_id) WHERE (status = 'scheduled'::text);

ALTER TABLE ONLY billing.subscription_reprices
    ADD CONSTRAINT subscription_reprices_batch_fk FOREIGN KEY (merchant_id, reprice_batch_id) REFERENCES billing.reprice_batches(merchant_id, id) ON DELETE SET NULL (reprice_batch_id);
ALTER TABLE ONLY billing.subscription_reprices
    ADD CONSTRAINT subscription_reprices_from_price_fk FOREIGN KEY (merchant_id, from_price_id) REFERENCES billing.prices(merchant_id, id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.subscription_reprices
    ADD CONSTRAINT subscription_reprices_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.subscription_reprices
    ADD CONSTRAINT subscription_reprices_subscription_fk FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.subscription_reprices
    ADD CONSTRAINT subscription_reprices_to_price_fk FOREIGN KEY (merchant_id, to_price_id) REFERENCES billing.prices(merchant_id, id) ON DELETE RESTRICT;

CREATE TABLE billing.solana_subscriptions (
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

ALTER TABLE ONLY billing.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_subscription_pda_key UNIQUE (subscription_pda);

CREATE INDEX idx_solana_subscriptions_due ON billing.solana_subscriptions USING btree (merchant_id, next_pull_at) WHERE (status = 'active'::text);
CREATE INDEX idx_solana_subscriptions_merchant_id ON billing.solana_subscriptions USING btree (merchant_id);
CREATE INDEX idx_solana_subscriptions_subscription_id ON billing.solana_subscriptions USING btree (subscription_id);

ALTER TABLE ONLY billing.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.solana_subscriptions
    ADD CONSTRAINT solana_subscriptions_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, id) ON DELETE CASCADE;

-- ---------------------------------------------------------------------------
-- Payments, checkout and attempts
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.enqueue_payment_settlement_event() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    IF NEW.status = 'completed'
       AND NEW.amount > 0
       AND NEW.refunded_payment_id IS NULL
       AND NEW.money_movement = 'rail'
       AND (TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM NEW.status)
    THEN
        INSERT INTO billing.host_outbox (merchant_id, event_type, subject_type, subject_id, payment_id, amount, currency, occurred_at, dedupe_key)
        VALUES (NEW.merchant_id, 'payment.settled', 'payment', NEW.id, NEW.id, NEW.amount, NEW.currency,
                COALESCE(NEW.purchased_at, NEW.created_at, now()), 'payment:' || NEW.id::text)
        ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE billing.payments (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid NOT NULL,
    rail text NOT NULL,
    transaction_id text NOT NULL,
    amount bigint NOT NULL,
    list_amount bigint NOT NULL,
    currency text NOT NULL,
    status billing.payment_status DEFAULT 'completed'::billing.payment_status NOT NULL,
    subscription_id uuid,
    refunded_payment_id uuid,
    discount_code text,
    discount_reason text,
    discount_metadata jsonb,
    entitlements_spec_snapshot jsonb,
    metadata jsonb,
    purchased_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    card_brand text,
    card_last4 text,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid,
    attempt_kind text,
    failure_code text,
    failure_reason text,
    reversal_kind text,
    token_type text,
    deleted_at timestamp with time zone,
    destructive_run_id uuid,
    destructive_run_class text GENERATED ALWAYS AS (CASE WHEN destructive_run_id IS NOT NULL THEN 'destructive' END) STORED,
    money_movement text DEFAULT 'none'::text NOT NULL,
    CONSTRAINT chk_payment_not_future CHECK ((purchased_at <= (now() + '00:05:00'::interval))),
    CONSTRAINT chk_payments_attempt_kind CHECK (((attempt_kind IS NULL) OR (attempt_kind = ANY (ARRAY['initial'::text, 'renewal'::text])))),
    CONSTRAINT chk_payments_money_movement CHECK ((money_movement = ANY (ARRAY['rail'::text, 'none'::text]))),
    CONSTRAINT chk_payments_reversal_kind CHECK (((reversal_kind IS NULL) OR (reversal_kind = ANY (ARRAY['refund'::text, 'chargeback'::text, 'dispute_reversal'::text])))),
    CONSTRAINT chk_payments_token_type CHECK (((token_type IS NULL) OR (token_type = ANY (ARRAY['network_token'::text, 'pan_via_proxy'::text, 'psp_token'::text])))),
    CONSTRAINT payments_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text))),
    CONSTRAINT payments_psp_required_on_rail CHECK (((psp_id IS NOT NULL) OR (rail = ANY (ARRAY['manual'::text, 'admin'::text]))))
);
COMMENT ON TABLE billing.payments IS 'Records of all payment transactions (formerly purchases table)';
COMMENT ON COLUMN billing.payments.subscription_id IS 'Links a payment to the subscription that generated it (nullable for one-off payments)';
COMMENT ON COLUMN billing.payments.psp_id IS 'PSP that took this charge. Required on every real rail (payments_psp_required_on_rail); NULL only for off-rail channels (manual/admin), which have no provider — or#893.';
COMMENT ON COLUMN billing.payments.attempt_kind IS '#733 initial|renewal, stamped at write time by the checkout vs rebill paths; NULL = unknown (imported/pre-instrumentation rows).';
COMMENT ON COLUMN billing.payments.failure_code IS '#733 raw rail decline code, recorded verbatim (no fabrication).';
COMMENT ON COLUMN billing.payments.failure_reason IS '#733 normalized decline category, derived deterministically from failure_code per rail.';
COMMENT ON COLUMN billing.payments.reversal_kind IS '#733 discriminates mirror rows: refund | chargeback | dispute_reversal (dispute won). NULL on sale rows.';
COMMENT ON COLUMN billing.payments.token_type IS '#796 credential form presented to the network: network_token | pan_via_proxy | psp_token. NULL = unknown/legacy; excluded from token_type-dimensioned metrics.';
COMMENT ON COLUMN billing.payments.deleted_at IS 'or#858 soft delete: set, the row is invisible to every live read. Only `pull-provider --prune` sets it, and `openrails undo-run` clears it.';
COMMENT ON COLUMN billing.payments.money_movement IS 'or#827 rail|none — positive marker for real money movement at the payment rail. ''rail'' rows carry a rail-issued transaction_id and are the ONLY rows the host settlement feed publishes; ''none'' rows are bookkeeping (attempt anchors, declines, placeholders). Fail-closed default: undeclared = ''none''.';

ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_merchant_payer_id_key UNIQUE (merchant_id, customer_id, id);
ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_merchant_payer_currency_id_key UNIQUE (merchant_id, customer_id, currency, id);
ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_merchant_id_id_key UNIQUE (merchant_id, id);

CREATE INDEX idx_payments_customer ON billing.payments USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE INDEX idx_payments_destructive_run ON billing.payments USING btree (destructive_run_id) WHERE (destructive_run_id IS NOT NULL);
CREATE INDEX idx_payments_merchant_created ON billing.payments USING btree (merchant_id, created_at DESC);
CREATE INDEX idx_payments_merchant_id ON billing.payments USING btree (merchant_id);
CREATE INDEX idx_payments_merchant_purchased ON billing.payments USING btree (merchant_id, purchased_at);
CREATE INDEX idx_payments_merchant_rail_transaction ON billing.payments USING btree (merchant_id, rail, transaction_id);
CREATE INDEX idx_payments_metadata_nmi_order ON billing.payments USING btree (merchant_id, ((metadata ->> 'nmi_subscription_order_id'::text))) WHERE ((metadata ->> 'nmi_subscription_order_id'::text) IS NOT NULL);
CREATE INDEX idx_payments_metadata_stripe_invoice ON billing.payments USING btree (merchant_id, ((metadata ->> 'stripe_invoice_id'::text))) WHERE ((metadata ->> 'stripe_invoice_id'::text) IS NOT NULL);
CREATE INDEX idx_payments_price_id ON billing.payments USING btree (price_id);
CREATE INDEX idx_payments_psp ON billing.payments USING btree (psp_id) WHERE (psp_id IS NOT NULL);
CREATE INDEX idx_payments_purchased_at ON billing.payments USING btree (purchased_at);
CREATE INDEX idx_payments_rail ON billing.payments USING btree (rail);
CREATE INDEX idx_payments_refunded_payment_id ON billing.payments USING btree (refunded_payment_id);
CREATE INDEX idx_payments_subscription_id ON billing.payments USING btree (subscription_id);
CREATE UNIQUE INDEX uq_payments_merchant_offrail_transaction ON billing.payments USING btree (merchant_id, rail, transaction_id) WHERE ((psp_id IS NULL) AND (deleted_at IS NULL));
CREATE UNIQUE INDEX uq_payments_merchant_psp_transaction ON billing.payments USING btree (merchant_id, rail, psp_id, transaction_id) WHERE ((psp_id IS NOT NULL) AND (deleted_at IS NULL));

ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_destructive_run_fk FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs(merchant_id, id, run_class) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices(merchant_id, id);
ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id);
ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_refunded_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, currency, refunded_payment_id) REFERENCES billing.payments(merchant_id, customer_id, currency, id);
ALTER TABLE ONLY billing.payments
    ADD CONSTRAINT payments_subscription_id_fkey FOREIGN KEY (merchant_id, customer_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, customer_id, id) ON DELETE SET NULL (subscription_id);

CREATE TRIGGER payments_enqueue_settlement_event AFTER INSERT OR UPDATE OF status ON billing.payments FOR EACH ROW EXECUTE FUNCTION billing.enqueue_payment_settlement_event();

CREATE TABLE billing.checkout_sessions (
    id uuid DEFAULT uuidv7() NOT NULL,
    price_id uuid,
    mode text NOT NULL,
    rail text NOT NULL,
    status text NOT NULL,
    amount bigint,
    currency text,
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
    psp_id uuid NOT NULL,
    deleted_at timestamp with time zone,
    destructive_run_id uuid,
    destructive_run_class text GENERATED ALWAYS AS (CASE WHEN destructive_run_id IS NOT NULL THEN 'destructive' END) STORED,
    routing_reason jsonb,
    CONSTRAINT checkout_sessions_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text))),
    CONSTRAINT checkout_sessions_mode_check CHECK ((mode = ANY (ARRAY['one_off'::text, 'subscription'::text, 'solana_cancel'::text, 'solana_tier_change'::text, 'payment_method'::text]))),
    CONSTRAINT checkout_sessions_monetary_terms CHECK (
      (mode = 'payment_method' AND price_id IS NULL AND amount IS NULL AND currency IS NULL AND payment_id IS NULL AND subscription_id IS NULL)
      OR (mode <> 'payment_method' AND price_id IS NOT NULL AND amount IS NOT NULL AND currency IS NOT NULL)
    )
);
COMMENT ON COLUMN billing.checkout_sessions.psp_id IS 'PSP selected for this provider checkout/session. Required (or#893).';
COMMENT ON COLUMN billing.checkout_sessions.deleted_at IS 'or#858 soft delete: set, the row is invisible to every live read. Only `pull-provider --prune` sets it, and `openrails undo-run` clears it.';
COMMENT ON COLUMN billing.checkout_sessions.routing_reason IS 'or#288 processor-routing decision trace, written once at creation: {policy: explicit|merchant|default, rule: matched merchant-rule index, selected: PSP key, rail, fallbacks: [remaining eligible PSP keys, ranked], skipped: [{selector, reason}]}. Skip reasons are PRE-CHARGE availability classes (not_armed, credentials_missing, link_missing, mode_unsupported, service_unavailable, ambiguous_selector, unknown_selector, resolve_failed); a decline is never one of them. NULL = created before the column existed.';

ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_pkey PRIMARY KEY (id);

CREATE INDEX checkout_sessions_expires_at_idx ON billing.checkout_sessions USING btree (expires_at);
CREATE INDEX idx_checkout_sessions_customer ON billing.checkout_sessions USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE INDEX idx_checkout_sessions_destructive_run ON billing.checkout_sessions USING btree (destructive_run_id) WHERE (destructive_run_id IS NOT NULL);
CREATE INDEX idx_checkout_sessions_merchant_id ON billing.checkout_sessions USING btree (merchant_id);
CREATE INDEX idx_checkout_sessions_payment_id ON billing.checkout_sessions USING btree (merchant_id, payment_id) WHERE (payment_id IS NOT NULL);
CREATE INDEX idx_checkout_sessions_psp ON billing.checkout_sessions USING btree (psp_id);
CREATE INDEX idx_checkout_sessions_subscription_id ON billing.checkout_sessions USING btree (merchant_id, subscription_id) WHERE (subscription_id IS NOT NULL);
CREATE INDEX ix_checkout_sessions_expirable ON billing.checkout_sessions USING btree (merchant_id, expires_at) WHERE ((expires_at IS NOT NULL) AND (deleted_at IS NULL) AND (status = ANY (ARRAY['created'::text, 'requires_action'::text])));
CREATE UNIQUE INDEX uq_checkout_sessions_merchant_psp_reference ON billing.checkout_sessions USING btree (merchant_id, rail, psp_id, reference) WHERE ((reference IS NOT NULL) AND (deleted_at IS NULL));
CREATE UNIQUE INDEX uq_checkout_sessions_merchant_psp_transaction ON billing.checkout_sessions USING btree (merchant_id, rail, psp_id, transaction_id) WHERE ((transaction_id IS NOT NULL) AND (deleted_at IS NULL));
CREATE UNIQUE INDEX uq_checkout_sessions_solana_signature ON billing.checkout_sessions USING btree (transaction_id)
WHERE rail = 'solana' AND transaction_id IS NOT NULL AND deleted_at IS NULL;

ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_destructive_run_fk FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs(merchant_id, id, run_class) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments(merchant_id, customer_id, id);
ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices(merchant_id, id);
ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id);
ALTER TABLE ONLY billing.checkout_sessions
    ADD CONSTRAINT checkout_sessions_subscription_id_fkey FOREIGN KEY (merchant_id, customer_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, customer_id, id);

CREATE TABLE billing.solana_pay_references (
    merchant_id uuid NOT NULL,
    reference text NOT NULL,
    checkout_session_id uuid NOT NULL,
    kind text NOT NULL,
    status text NOT NULL,
    settle_until timestamp with time zone NOT NULL,
    watch_until timestamp with time zone NOT NULL,
    next_poll_at timestamp with time zone NOT NULL,
    signature text,
    seen_until text,
    scan_stack text[] DEFAULT '{}'::text[] NOT NULL,
    scan_below text,
    built_transaction text,
    built_valid_height bigint,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT solana_pay_references_pkey PRIMARY KEY (merchant_id, reference),
    CONSTRAINT solana_pay_references_session_key UNIQUE (merchant_id, checkout_session_id),
    CONSTRAINT solana_pay_references_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE,
    CONSTRAINT chk_solana_pay_references_kind CHECK (kind IN ('purchase', 'subscribe', 'lifecycle')),
    CONSTRAINT chk_solana_pay_references_status CHECK (status IN ('pending', 'confirmed', 'expired')),
    CONSTRAINT chk_solana_pay_references_signature CHECK ((status = 'confirmed') = (signature IS NOT NULL)),
    CONSTRAINT chk_solana_pay_references_window CHECK (watch_until >= settle_until),
    CONSTRAINT chk_solana_pay_references_built CHECK ((built_transaction IS NULL) = (built_valid_height IS NULL))
);
COMMENT ON TABLE billing.solana_pay_references IS '#1086: one Solana Pay reference per checkout attempt. pending = awaiting a transfer landed by settle_until; confirmed = one signature credited (or mirrored); expired = nothing credited by settle_until. Purchase references stay watched until watch_until so a second or late transfer is recorded, then openrails.solana_pay_gc deletes the settled row. seen_until is the newest signature whose older history is fully processed; scan_stack holds the before-cursors of an unfinished walk down the history and scan_below the cursor whose older signatures were just processed, so no signature is ever skipped however many land on the reference; a reference is never collected mid-walk. built_transaction is the one transaction-request tx offered while its blockhash can still land.';

CREATE INDEX idx_solana_pay_references_due ON billing.solana_pay_references USING btree (next_poll_at);
CREATE INDEX idx_solana_pay_references_settled ON billing.solana_pay_references USING btree (watch_until) WHERE status <> 'pending';

CREATE TABLE billing.solana_pay_receipts (
    merchant_id uuid NOT NULL,
    reference text NOT NULL,
    signature text NOT NULL,
    checkout_session_id uuid NOT NULL,
    disposition text NOT NULL,
    review_reason text,
    recipient text NOT NULL,
    token_mint text NOT NULL,
    expected_amount bigint NOT NULL,
    received_amount bigint NOT NULL,
    payer text,
    landed_at timestamp with time zone,
    payment_id uuid,
    resolved_at timestamp with time zone,
    resolution text,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT solana_pay_receipts_pkey PRIMARY KEY (merchant_id, reference, signature),
    CONSTRAINT solana_pay_receipts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE,
    CONSTRAINT chk_solana_pay_receipts_disposition CHECK (
        disposition = 'credited' AND payment_id IS NOT NULL AND (review_reason IS NULL OR review_reason = 'overpaid')
        OR disposition = 'review' AND payment_id IS NULL AND review_reason IN ('already_paid', 'late', 'underpaid', 'session_closed', 'wrong_asset', 'unreadable', 'settle_failed')
        OR disposition = 'duplicate' AND payment_id IS NULL AND review_reason = 'claimed_elsewhere'
        OR disposition = 'ignored' AND payment_id IS NULL AND review_reason IS NULL
    ),
    CONSTRAINT chk_solana_pay_receipts_resolution CHECK ((resolved_at IS NULL) = (resolution IS NULL) AND (resolved_at IS NULL OR review_reason IS NOT NULL)),
    CONSTRAINT chk_solana_pay_receipts_amounts CHECK (expected_amount >= 0 AND received_amount >= 0)
);
COMMENT ON TABLE billing.solana_pay_receipts IS '#1086: every signature observed on a Solana Pay reference, recorded once. credited = the checkout was paid by it (overpaid flags the excess for refund); review = money that was not credited (already_paid, late, underpaid, session_closed, wrong_asset, unreadable, settle_failed) and needs a refund or operator decision, closed by resolved_at; duplicate = the transfer already settled another reference; ignored = no value to the merchant (deleted with its reference). A transfer to one recipient in one mint is credited or reviewed at most once across every reference. Unresolved reviews refuse the billing archive.';

CREATE UNIQUE INDEX uq_solana_pay_receipts_transfer ON billing.solana_pay_receipts USING btree (signature, recipient, token_mint) WHERE disposition IN ('credited', 'review');
CREATE INDEX idx_solana_pay_receipts_review ON billing.solana_pay_receipts USING btree (merchant_id, created_at) WHERE review_reason IS NOT NULL AND resolved_at IS NULL;

CREATE TABLE billing.rebill_cycles (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    owner text NOT NULL,
    due_at timestamp with time zone NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    missed_at timestamp with time zone,
    miss_reason text,
    CONSTRAINT rebill_cycles_pkey PRIMARY KEY (id),
    CONSTRAINT rebill_cycles_merchant_id_key UNIQUE (merchant_id, id),
    CONSTRAINT rebill_cycles_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE,
    CONSTRAINT rebill_cycles_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id),
    CONSTRAINT rebill_cycles_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id),
    CONSTRAINT chk_rebill_cycles_owner CHECK (owner IN ('engine', 'nmi_schedule', 'provider')),
    CONSTRAINT chk_rebill_cycles_amount CHECK (amount >= 0),
    CONSTRAINT chk_rebill_cycles_currency CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    CONSTRAINT chk_rebill_cycles_missed CHECK ((missed_at IS NULL) = (miss_reason IS NULL)),
    CONSTRAINT chk_rebill_cycles_miss_reason CHECK (miss_reason IN ('held', 'refused', 'method_unusable', 'not_attempted', 'provider_skipped', 'provider_stalled', 'provider_reversed', 'provider_unrecorded', 'schedule_gone'))
);
COMMENT ON TABLE billing.rebill_cycles IS '#1111 one expected rebill per (subscription, due_at): the moment its paid period came due. Its attempts are payment_attempts.cycle_id.';
COMMENT ON COLUMN billing.rebill_cycles.missed_at IS '#1112 when the cycle passed its owner''s deadline with no attempt; a later attempt still attaches to the cycle.';

CREATE UNIQUE INDEX uq_rebill_cycles_due ON billing.rebill_cycles USING btree (merchant_id, subscription_id, due_at);
CREATE INDEX idx_rebill_cycles_time ON billing.rebill_cycles USING btree (merchant_id, due_at);
CREATE INDEX idx_rebill_cycles_psp ON billing.rebill_cycles USING btree (merchant_id, psp_id, due_at);

CREATE TABLE billing.payment_attempts (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    kind text NOT NULL,
    owner text NOT NULL,
    card_entry text NOT NULL,
    source text NOT NULL,
    observed_via text NOT NULL,
    category text NOT NULL,
    reason text,
    action text,
    response_code text,
    response_text text,
    transaction_id text,
    avs_result text,
    cvv_result text,
    card_brand text,
    card_last4 text,
    token_type text,
    amount bigint NOT NULL,
    currency text,
    attempted_at timestamp with time zone NOT NULL,
    checkout_id uuid,
    checkout_target text,
    subscription_id uuid,
    payment_method_id uuid,
    payment_id uuid,
    rail_intent_id uuid,
    step text DEFAULT '' NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    cycle_id uuid,
    card_bin text,
    issuer_code text,
    issuer_text text,
    enriched_at timestamp with time zone,
    CONSTRAINT payment_attempts_pkey PRIMARY KEY (id),
    CONSTRAINT payment_attempts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE,
    CONSTRAINT payment_attempts_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id),
    CONSTRAINT payment_attempts_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id),
    CONSTRAINT chk_payment_attempts_kind CHECK (kind IN ('verify', 'initial', 'upgrade', 'rebill', 'dunning_retry', 'customer_retry', 'invoice')),
    CONSTRAINT chk_payment_attempts_owner CHECK (owner IN ('engine', 'nmi_schedule', 'provider', 'none')),
    CONSTRAINT chk_payment_attempts_card_entry CHECK (card_entry IN ('new', 'saved')),
    CONSTRAINT chk_payment_attempts_source CHECK (source IN ('openrails', 'provider_schedule', 'external')),
    CONSTRAINT chk_payment_attempts_observed_via CHECK (observed_via IN ('response', 'webhook', 'pull')),
    CONSTRAINT chk_payment_attempts_category CHECK (category IN ('approved', 'card_data', 'issuer_soft', 'issuer_hard', 'gateway_rule', 'system_error', 'unknown')),
    CONSTRAINT chk_payment_attempts_outcome CHECK ((category = 'approved') = (reason IS NULL AND action IS NULL)),
    CONSTRAINT chk_payment_attempts_action CHECK (action IS NULL OR action IN ('retry', 'fix_payment_method', 'non_recoverable')),
    CONSTRAINT chk_payment_attempts_amount CHECK (amount >= 0),
    CONSTRAINT chk_payment_attempts_currency CHECK (currency ~ '^[A-Z0-9]{3,12}$' OR (currency IS NULL AND amount = 0)),
    CONSTRAINT chk_payment_attempts_text CHECK (length(response_text) <= 128),
    CONSTRAINT chk_payment_attempts_last4 CHECK (card_last4 ~ '^[0-9]{4}$'),
    CONSTRAINT chk_payment_attempts_token_type CHECK (token_type IN ('network_token', 'pan_via_proxy', 'psp_token')),
    CONSTRAINT chk_payment_attempts_checkout CHECK ((checkout_id IS NULL) = (checkout_target IS NULL)),
    CONSTRAINT chk_payment_attempts_cycle CHECK ((kind IN ('rebill', 'dunning_retry', 'customer_retry')) = (cycle_id IS NOT NULL)),
    CONSTRAINT chk_payment_attempts_card_bin CHECK (card_bin ~ '^[0-9]{6,8}$'),
    CONSTRAINT chk_payment_attempts_issuer CHECK (length(issuer_code) <= 32 AND length(issuer_text) <= 128)
);
COMMENT ON TABLE billing.payment_attempts IS '#1110 one row per authorization answered by a PSP: the $0 card verification, sales, rebills and retries. Never the PAN or CVV. checkout_id groups one buyer''s attempts on one target (checkout_target: a price id or card_save) until the target is approved.';
COMMENT ON COLUMN billing.payment_attempts.issuer_code IS '#1114 the issuer''s raw answer (NMI processor_response_code); response_code is the gateway''s.';
COMMENT ON COLUMN billing.payment_attempts.enriched_at IS '#1114 when the row was filled from the PSP''s transaction read; NULL rows are read by the enrichment pass.';

CREATE UNIQUE INDEX uq_payment_attempts_transaction ON billing.payment_attempts USING btree (merchant_id, psp_id, transaction_id) WHERE transaction_id IS NOT NULL;
CREATE UNIQUE INDEX uq_payment_attempts_operation_step ON billing.payment_attempts USING btree (merchant_id, rail_intent_id, step) WHERE rail_intent_id IS NOT NULL;
CREATE INDEX idx_payment_attempts_time ON billing.payment_attempts USING btree (merchant_id, attempted_at);
CREATE INDEX idx_payment_attempts_checkout ON billing.payment_attempts USING btree (merchant_id, customer_id, checkout_target, attempted_at) WHERE checkout_target IS NOT NULL;
CREATE INDEX idx_payment_attempts_cycle ON billing.payment_attempts USING btree (merchant_id, cycle_id) WHERE cycle_id IS NOT NULL;
CREATE INDEX idx_payment_attempts_subscription ON billing.payment_attempts USING btree (merchant_id, subscription_id, attempted_at) WHERE subscription_id IS NOT NULL;
CREATE INDEX idx_payment_attempts_checkout_id ON billing.payment_attempts USING btree (merchant_id, checkout_id, attempted_at) WHERE checkout_id IS NOT NULL;
CREATE INDEX idx_payment_attempts_unenriched ON billing.payment_attempts USING btree (merchant_id, attempted_at)
    WHERE enriched_at IS NULL AND rail = 'nmi' AND transaction_id IS NOT NULL;

ALTER TABLE ONLY billing.payment_attempts
    ADD CONSTRAINT payment_attempts_cycle_fk FOREIGN KEY (merchant_id, cycle_id) REFERENCES billing.rebill_cycles(merchant_id, id);

CREATE VIEW billing.rebill_cycle_facts WITH (security_invoker = true) AS
SELECT c.merchant_id, c.id, c.subscription_id, c.customer_id, c.psp_id, c.rail, c.owner, c.due_at, c.amount, c.currency,
       c.missed_at, c.miss_reason, c.created_at,
       f.category AS first_category, f.reason AS first_reason, f.attempted_at AS first_at,
       w.id AS won_attempt_id, w.kind AS won_kind, w.source AS won_source, w.attempted_at AS won_at, w.ordinal AS won_ordinal,
       (c.missed_at IS NOT NULL OR COALESCE(f.category <> 'approved', false)) AS first_failed,
       CASE WHEN c.missed_at IS NOT NULL THEN 'missed'
            WHEN f.category IS NULL THEN 'pending'
            WHEN f.category = 'approved' THEN 'approved'
            WHEN f.category = 'system_error' THEN 'error'
            ELSE 'declined' END AS first_outcome,
       -- Collected, the subscription cancelled, or the dunning window (at most
       -- 14 days) passed: whichever comes first.
       LEAST(w.attempted_at, CASE WHEN s.cancelled_at IS NOT NULL THEN GREATEST(s.cancelled_at, c.due_at) END, c.due_at + interval '15 days') AS closed_at,
       CASE WHEN w.id IS NULL OR NOT (c.missed_at IS NOT NULL OR COALESCE(f.category <> 'approved', false)) THEN ''
            WHEN w.source = 'provider_schedule' THEN 'late_provider_charge'
            WHEN EXISTS (SELECT 1 FROM billing.payment_method_updates u
                          WHERE u.merchant_id = c.merchant_id AND u.payment_method_id = w.payment_method_id AND u.kind = 'updated'
                            AND u.at >= COALESCE(c.missed_at, f.attempted_at) AND u.at <= w.attempted_at) THEN 'updated_card'
            WHEN w.kind = 'customer_retry' THEN 'customer_retry'
            ELSE 'dunning_retry' END AS recovered_by
  FROM billing.rebill_cycles c
  LEFT JOIN LATERAL (SELECT a.category, a.reason, a.attempted_at FROM billing.payment_attempts a
                      WHERE a.merchant_id = c.merchant_id AND a.cycle_id = c.id ORDER BY a.attempted_at, a.id LIMIT 1) f ON true
  LEFT JOIN LATERAL (SELECT a.id, a.kind, a.source, a.attempted_at, a.payment_method_id,
                            (SELECT count(*) FROM billing.payment_attempts b
                              WHERE b.merchant_id = c.merchant_id AND b.cycle_id = c.id AND (b.attempted_at, b.id) <= (a.attempted_at, a.id)) AS ordinal
                       FROM billing.payment_attempts a
                      WHERE a.merchant_id = c.merchant_id AND a.cycle_id = c.id AND a.category = 'approved'
                      ORDER BY a.attempted_at, a.id LIMIT 1) w ON true
  LEFT JOIN billing.subscriptions s ON s.merchant_id = c.merchant_id AND s.id = c.subscription_id;
COMMENT ON VIEW billing.rebill_cycle_facts IS '#1116 each rebill cycle with its first attempt, the attempt that collected it and when it closes (collected, cancelled, or 15 days past due). A cycle is open until closed_at, lost when closed uncollected.';

CREATE TABLE billing.card_attempt_failures (
    merchant_id uuid NOT NULL,
    subject text NOT NULL,
    bucket_at timestamp with time zone NOT NULL,
    failures bigint NOT NULL,
    CONSTRAINT card_attempt_failures_pkey PRIMARY KEY (merchant_id, subject, bucket_at),
    CONSTRAINT card_attempt_failures_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE,
    CONSTRAINT chk_card_attempt_failures_positive CHECK (failures > 0),
    CONSTRAINT chk_card_attempt_failures_subject CHECK (subject <> '' AND length(subject) <= 200)
);
COMMENT ON TABLE billing.card_attempt_failures IS 'SEC-30 card-testing failure counts per merchant, subject and five-minute bucket.';

CREATE INDEX idx_card_attempt_failures_merchant_bucket ON billing.card_attempt_failures USING btree (merchant_id, bucket_at);

CREATE TABLE billing.idempotency_keys (
    merchant_id uuid NOT NULL,
    operation text NOT NULL,
    idempotency_key text NOT NULL,
    status text NOT NULL,
    token uuid NOT NULL,
    claims bigint DEFAULT 1 NOT NULL,
    result jsonb,
    error text,
    lease_expires_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (merchant_id, operation, idempotency_key),
    CONSTRAINT idempotency_keys_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE,
    CONSTRAINT chk_idempotency_keys_status CHECK (status IN ('processing', 'succeeded', 'failed')),
    CONSTRAINT chk_idempotency_keys_identity CHECK (operation <> '' AND idempotency_key <> ''),
    CONSTRAINT chk_idempotency_keys_result CHECK (status = 'succeeded' OR result IS NULL),
    CONSTRAINT chk_idempotency_keys_claims CHECK (claims > 0),
    CONSTRAINT chk_idempotency_keys_expiry CHECK (expires_at >= lease_expires_at)
);
COMMENT ON TABLE billing.idempotency_keys IS '#1099: one claim per (merchant, operation, key). processing = owned until lease_expires_at, then reclaimable by exactly one caller; succeeded = replay result; failed = reclaimable. token fences a superseded owner; claims counts claims. Rows past expires_at are deleted by openrails.idempotency_gc.';

CREATE INDEX idx_idempotency_keys_expires_at ON billing.idempotency_keys USING btree (expires_at);

-- ---------------------------------------------------------------------------
-- Ledger, grants and entitlements
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.guard_ledger_account_facts() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    -- Only a nested trigger may maintain counters. The existing transfer
    -- AFTER INSERT trigger is their sole writer; ordinary UPDATE cannot forge
    -- trigger nesting, and no caller-set session setting grants this permission.
    -- Keep the existing row locks/arithmetic: rescanning transfers here would
    -- change multi-row insertion and concurrent transfer snapshot semantics.
    IF TG_OP='DELETE' OR pg_trigger_depth()<2
       OR (to_jsonb(NEW)-ARRAY['debits_posted','credits_posted']) IS DISTINCT FROM
          (to_jsonb(OLD)-ARRAY['debits_posted','credits_posted']) THEN
        RAISE EXCEPTION 'ledger account facts are immutable; counters are maintained by transfer insertion' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE billing.ledger_accounts (
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
    CONSTRAINT ledger_accounts_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text))),
    CONSTRAINT ledger_accounts_type_check CHECK ((account_type = ANY (ARRAY['customer_balance'::text, 'platform_revenue'::text, 'processor_clearing'::text, 'arrears_liability'::text, 'expired_credits'::text, 'revoked_credits'::text, 'fx_liquidity'::text, 'world'::text])))
);
COMMENT ON TABLE billing.ledger_accounts IS '#512 double-entry ledger accounts. One account belongs to exactly one (merchant, currency) ledger; TB-style posted/pending counters are maintained from immutable ledger_transfers and verified by reconciliation. account_type identifies its role (customer_balance, platform_revenue, processor_clearing, arrears_liability, expired_credits, fx_liquidity, world).';
COMMENT ON COLUMN billing.ledger_accounts.customer_id IS 'NULL for system accounts (one per merchant+currency); set for per-customer balance accounts.';
COMMENT ON COLUMN billing.ledger_accounts.account_type IS 'Account role within a (merchant, currency) ledger. arrears_liability is PER-CUSTOMER (or#897): its negated balance is that payer''s outstanding owed, read O(1) on the admission path. customer_balance is per-customer; processor_clearing / platform_revenue / expired_credits / revoked_credits / fx_liquidity / world are merchant-wide system accounts.';
COMMENT ON COLUMN billing.ledger_accounts.debits_must_not_exceed_credits IS 'TB sign flag: balance (credits-debits) may not go below zero (minus an applier-supplied arrears floor). Set on customer_balance.';
COMMENT ON COLUMN billing.ledger_accounts.credits_posted IS 'Phase H maintained counter: posted credits for O(1) balance reads.';
COMMENT ON COLUMN billing.ledger_accounts.debits_posted IS 'Phase H maintained counter: posted debits for O(1) balance reads.';

ALTER TABLE ONLY billing.ledger_accounts
    ADD CONSTRAINT ledger_accounts_merchant_id_id_key UNIQUE (merchant_id, id);
ALTER TABLE ONLY billing.ledger_accounts
    ADD CONSTRAINT ledger_accounts_merchant_payer_id_key UNIQUE (merchant_id, customer_id, id);
ALTER TABLE ONLY billing.ledger_accounts
    ADD CONSTRAINT ledger_accounts_pkey PRIMARY KEY (id);

CREATE INDEX idx_ledger_accounts_customer ON billing.ledger_accounts USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE INDEX idx_ledger_accounts_merchant_id ON billing.ledger_accounts USING btree (merchant_id);
CREATE UNIQUE INDEX uq_ledger_accounts_customer ON billing.ledger_accounts USING btree (merchant_id, customer_id, account_type, currency) WHERE (customer_id IS NOT NULL);
CREATE UNIQUE INDEX uq_ledger_accounts_system ON billing.ledger_accounts USING btree (merchant_id, account_type, currency) WHERE (customer_id IS NULL);

ALTER TABLE ONLY billing.ledger_accounts
    ADD CONSTRAINT ledger_accounts_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.ledger_accounts
    ADD CONSTRAINT ledger_accounts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TRIGGER guard_ledger_account_facts BEFORE UPDATE OR DELETE ON billing.ledger_accounts
FOR EACH ROW EXECUTE FUNCTION billing.guard_ledger_account_facts();
CREATE TRIGGER immutable_ledger_accounts_truncate BEFORE TRUNCATE ON billing.ledger_accounts
EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE FUNCTION billing.ledger_transfers_apply_counters() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
DECLARE
    acc billing.ledger_accounts%ROWTYPE;
    debit billing.ledger_accounts%ROWTYPE;
    credit billing.ledger_accounts%ROWTYPE;
    debit_balance bigint;
    credit_balance bigint;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    FOR acc IN
        SELECT *
        FROM billing.ledger_accounts
        WHERE merchant_id = NEW.merchant_id
          AND id IN (NEW.debit_account_id, NEW.credit_account_id)
        ORDER BY id
        -- Counters do not change account keys. FK checks may already hold
        -- KEY SHARE locks; upgrading those to FOR UPDATE can deadlock peers.
        FOR NO KEY UPDATE
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

    UPDATE billing.ledger_accounts
    SET debits_posted = debits_posted + NEW.amount
    WHERE merchant_id = NEW.merchant_id AND id = NEW.debit_account_id;
    UPDATE billing.ledger_accounts
    SET credits_posted = credits_posted + NEW.amount
    WHERE merchant_id = NEW.merchant_id AND id = NEW.credit_account_id;

    RETURN NEW;
END;
$$;

CREATE TABLE billing.ledger_transfers (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    debit_account_id uuid NOT NULL,
    credit_account_id uuid NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    transfer_type text NOT NULL,
    allow_debit_negative_up_to bigint DEFAULT 0 NOT NULL,
    source text NOT NULL,
    source_id text NOT NULL,
    grant_id uuid,
    customer_id uuid,
    invoker_id text,
    resource text,
    invoice_id uuid,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    operation text NOT NULL,
    CONSTRAINT chk_ledger_transfers_coordinate_not_blank CHECK (((operation <> ''::text) AND (source <> ''::text) AND (source_id <> ''::text))),
    CONSTRAINT chk_ledger_transfers_source_present CHECK (((source IS NOT NULL) AND (source_id IS NOT NULL))),
    CONSTRAINT ledger_transfers_amount_positive CHECK ((amount > 0)),
    CONSTRAINT ledger_transfers_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text))),
    CONSTRAINT ledger_transfers_debit_floor_nonnegative CHECK ((allow_debit_negative_up_to >= 0)),
    CONSTRAINT ledger_transfers_distinct_accounts CHECK ((debit_account_id <> credit_account_id)),
    CONSTRAINT ledger_transfers_type_check CHECK ((transfer_type = ANY (ARRAY['deposit'::text, 'credit_spend'::text, 'credit_expire'::text, 'credit_revoke'::text, 'credit_reinstate'::text, 'owed_accrual'::text, 'owed_payment'::text, 'owed_writeoff'::text])))
);
COMMENT ON TABLE billing.ledger_transfers IS '#512 immutable double-entry transfers. Append-only (role granted SELECT,INSERT only). A transfer moves amount debit->credit within ONE (merchant, currency) ledger; capture/void/refund/expiry are NEW rows, never updates. ledger_accounts counters are a maintained projection of this table.';
COMMENT ON COLUMN billing.ledger_transfers.allow_debit_negative_up_to IS 'Debit-account floor used by the counter trigger for debits_must_not_exceed_credits accounts. Usually 0; arrears paths pass the current credit-line allowance.';
COMMENT ON COLUMN billing.ledger_transfers.source IS 'Opaque origin key (e.g. ''grant''/grant_id, ''payment''/transaction_id). Ledger purity: business joins live in control-plane tables.';
COMMENT ON COLUMN billing.ledger_transfers.grant_id IS 'Credit-lot attribution. grant_id/invoice_id/customer_id deliberately carry NO FKs (ledger purity, #709): the append-only ledger never blocks or cascades on control-plane rows.';
COMMENT ON COLUMN billing.ledger_transfers.operation IS 'or#894 engine-composed money-operation kind (capture / spend / withdraw / usage:<event_type> / deposit / ...). Part of the idempotency coordinate together with (source, source_id): two different operations sharing a caller key must not alias.';

ALTER TABLE ONLY billing.ledger_transfers
    ADD CONSTRAINT ledger_transfers_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.ledger_transfers
    ADD CONSTRAINT ledger_transfers_merchant_payer_currency_id_key UNIQUE (merchant_id, customer_id, currency, id);
ALTER TABLE ONLY billing.ledger_transfers
    ADD CONSTRAINT ledger_transfers_merchant_id_id_key UNIQUE (merchant_id, id);

CREATE INDEX idx_ledger_transfers_credit ON billing.ledger_transfers USING btree (credit_account_id);
CREATE INDEX idx_ledger_transfers_customer ON billing.ledger_transfers USING btree (merchant_id, customer_id, currency, created_at DESC) WHERE (customer_id IS NOT NULL);
CREATE INDEX idx_ledger_transfers_debit ON billing.ledger_transfers USING btree (debit_account_id);
CREATE INDEX idx_ledger_transfers_grant ON billing.ledger_transfers USING btree (merchant_id, grant_id) WHERE (grant_id IS NOT NULL);
CREATE UNIQUE INDEX idx_ledger_transfers_lot_once ON billing.ledger_transfers USING btree (merchant_id, grant_id, transfer_type) WHERE ((grant_id IS NOT NULL) AND (transfer_type = ANY (ARRAY['deposit'::text, 'credit_expire'::text, 'credit_revoke'::text])));
CREATE INDEX idx_ledger_transfers_merchant_created ON billing.ledger_transfers USING btree (merchant_id, created_at);
CREATE INDEX idx_ledger_transfers_merchant_id ON billing.ledger_transfers USING btree (merchant_id);
CREATE UNIQUE INDEX idx_ledger_transfers_operation_once ON billing.ledger_transfers USING btree (merchant_id, customer_id, currency, transfer_type, operation, source, source_id, grant_id) NULLS NOT DISTINCT;
CREATE INDEX idx_ledger_transfers_source ON billing.ledger_transfers USING btree (merchant_id, source, source_id) WHERE (source IS NOT NULL);

ALTER TABLE ONLY billing.ledger_transfers
    ADD CONSTRAINT ledger_transfers_credit_fk FOREIGN KEY (merchant_id, credit_account_id) REFERENCES billing.ledger_accounts(merchant_id, id);
ALTER TABLE ONLY billing.ledger_transfers
    ADD CONSTRAINT ledger_transfers_debit_fk FOREIGN KEY (merchant_id, debit_account_id) REFERENCES billing.ledger_accounts(merchant_id, id);
ALTER TABLE ONLY billing.ledger_transfers
    ADD CONSTRAINT ledger_transfers_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TRIGGER trg_ledger_transfers_apply_counters AFTER INSERT ON billing.ledger_transfers FOR EACH ROW EXECUTE FUNCTION billing.ledger_transfers_apply_counters();
CREATE TRIGGER immutable_ledger_transfers BEFORE UPDATE OR DELETE ON billing.ledger_transfers
FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_ledger_transfers_truncate BEFORE TRUNCATE ON billing.ledger_transfers
EXECUTE FUNCTION billing.reject_immutable_billing_fact();

COMMENT ON INDEX billing.idx_ledger_transfers_operation_once IS 'or#892: the structural once-only key for EVERY transfer type. ledger.ApplyIdempotent inserts ON CONFLICT DO NOTHING against this index, so a replay is refused by the database rather than by a check-then-insert in Go. grant_id is part of the identity (one debit per operation per FIFO lot); NULLS NOT DISTINCT because the owed/payment legs carry no lot and system transfers carry no customer.';

CREATE TABLE billing.grants (
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
    CONSTRAINT grants_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text))),
    CONSTRAINT grants_event_check CHECK ((event = ANY (ARRAY['grant'::text, 'revoke'::text, 'expire'::text, 'supersede'::text, 'adjust'::text]))),
    CONSTRAINT grants_event_supersedes CHECK (((event = 'grant'::text) = (supersedes_id IS NULL))),
    CONSTRAINT grants_kind_check CHECK ((kind = ANY (ARRAY['entitlement'::text, 'ownership'::text, 'credit'::text]))),
    CONSTRAINT grants_source_type_check CHECK ((source_type = ANY (ARRAY['purchase'::text, 'subscription'::text, 'admin'::text, 'grace'::text]))),
    CONSTRAINT grants_termination_no_window CHECK (((event = 'grant'::text) OR (ends_at IS NULL))),
    CONSTRAINT grants_valid_window CHECK (((ends_at IS NULL) OR (starts_at < ends_at)))
);
COMMENT ON TABLE billing.grants IS '#514 append-only grant ledger: the access-domain sibling of the #512 money ledger. Immutable events (grant/revoke/expire/supersede/adjust); the live entitlement windows, product ownership, and credit lots are DERIVED projections folded from this log. A credit grant carries the lot amount+currency and IS the FIFO credit lot (subsumes the old money_blocks role); derive-2 emits its #512 deposit transfer tagged source=grant.';
COMMENT ON COLUMN billing.grants.event IS 'grant roots a grant; revoke/expire/supersede/adjust are new rows referencing it via supersedes_id. The grant row is never updated.';
COMMENT ON COLUMN billing.grants.spec_snapshot IS 'Product entitlements/credits spec captured at issuance so derive-2 (grant->projection) is a pure function and replay is exact + historical.';

ALTER TABLE ONLY billing.grants
    ADD CONSTRAINT grants_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.grants
    ADD CONSTRAINT grants_merchant_payer_id_key UNIQUE (merchant_id, customer_id, id);
ALTER TABLE ONLY billing.grants
    ADD CONSTRAINT grants_merchant_id_id_key UNIQUE (merchant_id, id);

CREATE INDEX idx_grants_credit_customer_currency ON billing.grants USING btree (merchant_id, customer_id, currency, starts_at, ends_at) WHERE ((kind = 'credit'::text) AND (event = 'grant'::text));
CREATE INDEX idx_grants_credit_expiry ON billing.grants USING btree (merchant_id, ends_at) WHERE ((kind = 'credit'::text) AND (event = 'grant'::text) AND (ends_at IS NOT NULL));
CREATE INDEX idx_grants_customer ON billing.grants USING btree (merchant_id, customer_id);
CREATE INDEX idx_grants_customer_credit_currency ON billing.grants USING btree (merchant_id, customer_id, currency) WHERE ((kind = 'credit'::text) AND (event = 'grant'::text));
CREATE INDEX idx_grants_customer_kind ON billing.grants USING btree (merchant_id, customer_id, kind) WHERE (event = 'grant'::text);
CREATE INDEX idx_grants_merchant_credit_created ON billing.grants USING btree (merchant_id, created_at) WHERE (kind = 'credit'::text);
CREATE INDEX idx_grants_merchant_id ON billing.grants USING btree (merchant_id);
CREATE INDEX idx_grants_payment_id ON billing.grants USING btree (merchant_id, payment_id) WHERE (payment_id IS NOT NULL);
CREATE INDEX idx_grants_source ON billing.grants USING btree (merchant_id, source_type, source_id) WHERE (source_id <> ''::text);
CREATE INDEX idx_grants_supersedes ON billing.grants USING btree (supersedes_id) WHERE (supersedes_id IS NOT NULL);
CREATE UNIQUE INDEX uq_grants_credit_deposit_once ON billing.grants USING btree (merchant_id, customer_id, source_id) WHERE ((kind = 'credit'::text) AND (event = 'grant'::text) AND (source_id <> ''::text));
CREATE UNIQUE INDEX uq_grants_termination ON billing.grants USING btree (supersedes_id) WHERE ((supersedes_id IS NOT NULL) AND (event = ANY (ARRAY['revoke'::text, 'expire'::text, 'supersede'::text])));

ALTER TABLE ONLY billing.grants
    ADD CONSTRAINT grants_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.grants
    ADD CONSTRAINT grants_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.grants
    ADD CONSTRAINT grants_payment_fk FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments(merchant_id, customer_id, id);
ALTER TABLE ONLY billing.grants
    ADD CONSTRAINT grants_product_fk FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products(merchant_id, id);
ALTER TABLE ONLY billing.grants
    ADD CONSTRAINT grants_supersedes_fk FOREIGN KEY (merchant_id, customer_id, supersedes_id) REFERENCES billing.grants(merchant_id, customer_id, id);

CREATE TRIGGER immutable_grants BEFORE UPDATE OR DELETE ON billing.grants
FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_grants_truncate BEFORE TRUNCATE ON billing.grants
EXECUTE FUNCTION billing.reject_immutable_billing_fact();

COMMENT ON CONSTRAINT grants_termination_no_window ON billing.grants IS 'Only grant events carry an access window; revoke/expire/supersede are window-less point events (valid-time instant on starts_at, transaction time on created_at).';
COMMENT ON INDEX billing.uq_grants_credit_deposit_once IS 'LED-17 (or#906): a deposit happens at most once at the caller''s key (merchant, customer, source_id). Merchant-led (ID-11). source_type is NOT part of the key — the same source_id under a different source label is the same deposit. Once-only is a database fact, not a consequence of depositTx''s lockBalance serialization.';

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
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    grant_id uuid,
    destructive_run_id uuid,
    destructive_run_class text GENERATED ALWAYS AS (CASE WHEN destructive_run_id IS NOT NULL THEN 'destructive' END) STORED,
    CONSTRAINT chk_entitlements_source_type CHECK ((source_type = ANY (ARRAY['subscription'::text, 'one_off'::text, 'admin'::text, 'grace'::text, 'grant'::text]))),
    CONSTRAINT chk_revoke_fields_together CHECK (((revoked_at IS NULL) = (revoke_reason IS NULL))),
    CONSTRAINT chk_valid_time_window CHECK (((end_at IS NULL) OR (start_at < end_at)))
);
COMMENT ON COLUMN billing.entitlements.customer_id IS 'OpenRails payable tenant subject for this entitlement window.';

-- Source-owned intervals may overlap. Reads derive their union by existence.
ALTER TABLE ONLY billing.entitlements
    ADD CONSTRAINT entitlements_pkey PRIMARY KEY (id);

CREATE INDEX idx_entitlements_closed_at ON billing.entitlements USING btree (merchant_id, LEAST(COALESCE(end_at, 'infinity'::timestamp with time zone), COALESCE(revoked_at, 'infinity'::timestamp with time zone))) WHERE ((end_at IS NOT NULL) OR (revoked_at IS NOT NULL));
CREATE INDEX idx_entitlements_customer_active_window ON billing.entitlements USING btree (merchant_id, customer_id, entitlement, start_at, end_at) WHERE ((customer_id IS NOT NULL) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX idx_entitlements_destructive_run ON billing.entitlements USING btree (destructive_run_id) WHERE (destructive_run_id IS NOT NULL);
CREATE INDEX idx_entitlements_grace_by_subscription_live ON billing.entitlements USING btree (source_id, entitlement, start_at, end_at) WHERE ((source_type = 'grace'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX idx_entitlements_grant_id ON billing.entitlements USING btree (grant_id) WHERE (grant_id IS NOT NULL);
CREATE INDEX idx_entitlements_live_by_id ON billing.entitlements USING btree (id) WHERE ((revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX idx_entitlements_merchant_id ON billing.entitlements USING btree (merchant_id);
CREATE INDEX idx_entitlements_one_off_source_live ON billing.entitlements USING btree (source_id, entitlement) WHERE ((source_type = 'one_off'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX idx_entitlements_reverse_active ON billing.entitlements USING btree (merchant_id, entitlement, customer_id) WHERE ((revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE INDEX idx_entitlements_source ON billing.entitlements USING btree (source_type, source_id) WHERE (source_id IS NOT NULL);
CREATE INDEX idx_entitlements_subscription_source_live ON billing.entitlements USING btree (source_id, entitlement, end_at) WHERE ((source_type = 'subscription'::text) AND (revoked_at IS NULL) AND (deleted_at IS NULL));
CREATE UNIQUE INDEX uq_entitlements_grant_feature ON billing.entitlements (merchant_id, grant_id, entitlement)
    WHERE grant_id IS NOT NULL AND deleted_at IS NULL;

ALTER TABLE ONLY billing.entitlements
    ADD CONSTRAINT entitlements_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.entitlements
    ADD CONSTRAINT entitlements_destructive_run_fk FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs(merchant_id, id, run_class) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.entitlements
    ADD CONSTRAINT entitlements_grant_fk FOREIGN KEY (merchant_id, customer_id, grant_id) REFERENCES billing.grants(merchant_id, customer_id, id);
ALTER TABLE ONLY billing.entitlements
    ADD CONSTRAINT entitlements_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

-- ---------------------------------------------------------------------------
-- Billing policies, usage and admission
-- ---------------------------------------------------------------------------

CREATE TABLE billing.billing_policies (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    policy jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
COMMENT ON TABLE billing.billing_policies IS 'or#897: the merchant''s named billing policies. The policy body declares WHICH quantity is capped (kind=outstanding_cap | window_spend_cap | accrual_rate_cap) and the limit. Merchants bind names to customers/tiers via billing_policy_bindings; OpenRails enforces, the merchant decides who gets which.';
COMMENT ON COLUMN billing.billing_policies.policy IS 'JSONB policy body: kind, the kind''s limit (outstanding_cap_amount micros / spend_windows), bad_spend_windows (#497 wasted-spend grace) and policy_currency. Validated by ONE normalizer shared by the manifest loader and the config API.';

ALTER TABLE ONLY billing.billing_policies
    ADD CONSTRAINT billing_policies_name_key UNIQUE (merchant_id, name);
ALTER TABLE ONLY billing.billing_policies
    ADD CONSTRAINT billing_policies_pkey PRIMARY KEY (id);

ALTER TABLE ONLY billing.billing_policies
    ADD CONSTRAINT billing_policies_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TRIGGER lock_merchant_billing_policy BEFORE INSERT OR UPDATE OR DELETE ON billing.billing_policies FOR EACH ROW EXECUTE FUNCTION billing.lock_merchant_configuration_write();

CREATE TABLE billing.billing_policy_bindings (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid,
    tier text,
    policy_name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT billing_policy_bindings_rung_ck CHECK (((customer_id IS NULL) OR (tier IS NULL)))
);
COMMENT ON TABLE billing.billing_policy_bindings IS 'or#897: which named policy applies to whom. Three rungs, most specific wins: per-customer (customer_id set) > per-tier (tier set) > merchant default (both NULL). The binding is JUST a name reference — rebinding is the merchant''s runtime lever and moves no money.';
COMMENT ON COLUMN billing.billing_policy_bindings.tier IS 'Trust tier this binding applies to (the surviving rung of the retired payer_spend_limits.tier). NULL on the customer and default rungs.';

ALTER TABLE ONLY billing.billing_policy_bindings
    ADD CONSTRAINT billing_policy_bindings_pkey PRIMARY KEY (id);

CREATE INDEX idx_billing_policy_bindings_merchant_id ON billing.billing_policy_bindings USING btree (merchant_id);
CREATE UNIQUE INDEX uq_billing_policy_bindings_customer ON billing.billing_policy_bindings USING btree (merchant_id, customer_id) WHERE (customer_id IS NOT NULL);
CREATE UNIQUE INDEX uq_billing_policy_bindings_default ON billing.billing_policy_bindings USING btree (merchant_id) WHERE ((customer_id IS NULL) AND (tier IS NULL));
CREATE UNIQUE INDEX uq_billing_policy_bindings_tier ON billing.billing_policy_bindings USING btree (merchant_id, tier) WHERE ((customer_id IS NULL) AND (tier IS NOT NULL));

ALTER TABLE ONLY billing.billing_policy_bindings
    ADD CONSTRAINT billing_policy_bindings_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.billing_policy_bindings
    ADD CONSTRAINT billing_policy_bindings_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.billing_policy_bindings
    ADD CONSTRAINT billing_policy_bindings_policy_fk FOREIGN KEY (merchant_id, policy_name) REFERENCES billing.billing_policies(merchant_id, name) ON DELETE RESTRICT;

CREATE TRIGGER lock_merchant_policy_binding BEFORE INSERT OR UPDATE OR DELETE ON billing.billing_policy_bindings FOR EACH ROW EXECUTE FUNCTION billing.lock_merchant_configuration_write();

CREATE TABLE billing.money_settings (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    billing_mode text DEFAULT 'prepaid'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    tier text,
    currency text NOT NULL,
    credit_limit_amount bigint DEFAULT 0 NOT NULL,
    collection_payment_method_id uuid,
    CONSTRAINT money_settings_billing_mode_chk CHECK ((billing_mode = ANY (ARRAY['prepaid'::text, 'arrears'::text]))),
    CONSTRAINT money_settings_credit_limit_amount_nonneg_chk CHECK ((credit_limit_amount >= 0)),
    CONSTRAINT money_settings_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text)))
);
COMMENT ON TABLE billing.money_settings IS 'Per-(merchant, customer, currency) spend policy and money-in config. Amount values use the row currency internal precision. Admission reads billing_mode + credit_limit_amount + the ledger balance; per-invoker caps live in payer/invoker_spend_limits; arrears owed exposure is derived from open invoices.';
COMMENT ON COLUMN billing.money_settings.currency IS 'System currency code (USD/EUR/JPY); the Go registry is the authority. Stablecoins and crypto tokens are payment assets, not account currencies.';
COMMENT ON COLUMN billing.money_settings.credit_limit_amount IS 'Admin-set arrears credit line in the row currency internal precision. 0 = no arrears capacity; prepaid balance may still be spent.';

ALTER TABLE ONLY billing.money_settings
    ADD CONSTRAINT money_settings_pkey PRIMARY KEY (merchant_id, customer_id, currency);

ALTER TABLE ONLY billing.money_settings
    ADD CONSTRAINT money_settings_collection_payment_method_id_fkey FOREIGN KEY (merchant_id, customer_id, collection_payment_method_id) REFERENCES billing.payment_methods(merchant_id, customer_id, id) ON DELETE SET NULL (collection_payment_method_id);
ALTER TABLE ONLY billing.money_settings
    ADD CONSTRAINT money_settings_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.money_settings
    ADD CONSTRAINT money_settings_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TABLE billing.invoker_spend_limits (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    scope text NOT NULL,
    scope_key text DEFAULT ''::text NOT NULL,
    windows jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    provenance text DEFAULT ''::text NOT NULL,
    CONSTRAINT invoker_spend_limits_scope_check CHECK ((scope = ANY (ARRAY['invoker'::text, 'role'::text, 'invoker_tier'::text])))
);
COMMENT ON TABLE billing.invoker_spend_limits IS 'Per-invoker spend limits (#473/#517): the payer caps how much a delegated invoker/role can spend of the payer''s money. {scope, scope_key, windows[]} composed in one admit verdict over the payer balance. Payer-set only.';
COMMENT ON COLUMN billing.invoker_spend_limits.scope_key IS 'Immutable scope discriminator: role uuid (scope=role), invoker string (scope=invoker), or tier key (scope=invoker_tier).';
COMMENT ON COLUMN billing.invoker_spend_limits.provenance IS 'Opaque caller-supplied provenance reference (or#911), e.g. a signed-document digest. Stored verbatim, returned on reads; never interpreted.';

ALTER TABLE ONLY billing.invoker_spend_limits
    ADD CONSTRAINT invoker_spend_limits_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.invoker_spend_limits
    ADD CONSTRAINT invoker_spend_limits_uniq UNIQUE (merchant_id, customer_id, scope, scope_key);

ALTER TABLE ONLY billing.invoker_spend_limits
    ADD CONSTRAINT invoker_spend_limits_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.invoker_spend_limits
    ADD CONSTRAINT invoker_spend_limits_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TABLE billing.usage_events (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    invoker_id text NOT NULL,
    currency text NOT NULL,
    resource text,
    event_type text NOT NULL,
    dimensions jsonb DEFAULT '{}'::jsonb NOT NULL,
    amount bigint NOT NULL,
    source text NOT NULL,
    source_id text NOT NULL,
    ledger_transfer_id uuid,
    -- Pricing authority: catalog rows are metered inputs; host rows already carry final money.
    pricing_authority text NOT NULL CHECK (pricing_authority IN ('host', 'catalog')),
    metadata jsonb,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT usage_events_amount_check CHECK ((amount >= 0)),
    CONSTRAINT usage_events_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text)))
);
COMMENT ON TABLE billing.usage_events IS 'Append-only multi-dimensional metered usage (issue #289). Source of truth for usage reporting + #303 invoice line items. Host-priced (amount sent by the host); event + ledger debit commit in one tx. The hot admission path (#298) never reads this table.';
COMMENT ON COLUMN billing.usage_events.invoker_id IS 'Caller-supplied principal string that fired this metered usage event. Opaque to OpenRails; attribution + grouping only, not a FK. Joins use source/source_id.';
COMMENT ON COLUMN billing.usage_events.currency IS 'Native OpenRails currency code; amount uses this currency internal precision.';
COMMENT ON COLUMN billing.usage_events.pricing_authority IS 'host = amount is final host-priced settlement and must not be catalog-rated; catalog = amount is a metered input for catalog rating. Capture writes host, including zero-cost captures; RecordUsage writes host for positive amounts and catalog for zero-cost meter inputs.';
COMMENT ON COLUMN billing.usage_events.resource IS 'Caller-supplied free-form string for what was metered (tensorhub: endpoint slug; doujins: plan/item slug). Opaque to OpenRails; nullable, not a FK.';

ALTER TABLE ONLY billing.usage_events
    ADD CONSTRAINT usage_events_pkey PRIMARY KEY (id);

CREATE INDEX idx_usage_events_customer_time ON billing.usage_events USING btree (customer_id, occurred_at);
CREATE INDEX idx_usage_events_invoker ON billing.usage_events USING btree (merchant_id, invoker_id, occurred_at DESC);
CREATE INDEX idx_usage_events_merchant_occurred ON billing.usage_events USING btree (merchant_id, occurred_at);
CREATE INDEX ix_usage_events_payer_time ON billing.usage_events USING btree (merchant_id, customer_id, occurred_at);
CREATE INDEX ix_usage_events_payer_type_time ON billing.usage_events USING btree (merchant_id, customer_id, event_type, occurred_at);
CREATE UNIQUE INDEX uq_usage_events_idem ON billing.usage_events USING btree (merchant_id, customer_id, currency, event_type, source, source_id);

ALTER TABLE ONLY billing.usage_events
    ADD CONSTRAINT usage_events_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.usage_events
    ADD CONSTRAINT usage_events_ledger_transfer_fk FOREIGN KEY (merchant_id, customer_id, currency, ledger_transfer_id) REFERENCES billing.ledger_transfers(merchant_id, customer_id, currency, id);
ALTER TABLE ONLY billing.usage_events
    ADD CONSTRAINT usage_events_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TABLE billing.metered_rating_watermarks (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    source text NOT NULL,
    period_from timestamp with time zone NOT NULL,
    rated_through timestamp with time zone NOT NULL,
    accrued_amount bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT metered_rating_watermarks_accrued_nonneg CHECK ((accrued_amount >= 0)),
    CONSTRAINT metered_rating_watermarks_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text)))
);
COMMENT ON TABLE billing.metered_rating_watermarks IS '#672 per-period metered-rating watermark: cumulative accrued amount + rated-through cutoff per (payer, currency, meter source, period start), so overlapping invoice closes bill each unit of usage exactly once.';
COMMENT ON COLUMN billing.metered_rating_watermarks.source IS 'Meter accrual source key (metered:<meter>[:rate_card:<id>][:dim:<value>]).';
COMMENT ON COLUMN billing.metered_rating_watermarks.accrued_amount IS 'Micros already accrued for [period_from, rated_through); the sweep accrues only the delta above this.';

ALTER TABLE ONLY billing.metered_rating_watermarks
    ADD CONSTRAINT metered_rating_watermarks_pkey PRIMARY KEY (merchant_id, customer_id, currency, source, period_from);

ALTER TABLE ONLY billing.metered_rating_watermarks
    ADD CONSTRAINT metered_rating_watermarks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

-- One derived financial hold total, shared by every spend and authorization path.
CREATE FUNCTION billing.financial_held_amount(merchant uuid, payer uuid, unit text, as_of timestamptz)
RETURNS bigint LANGUAGE sql STABLE SECURITY INVOKER AS $$
    SELECT (
        COALESCE((SELECT SUM(oa.authorized_usd_micros)
            FROM billing.operation_authorizations oa
            JOIN billing.ledger_accounts a ON a.merchant_id = oa.merchant_id AND a.id = oa.ledger_account_id
            WHERE oa.merchant_id = merchant AND a.customer_id = payer AND a.currency = unit AND oa.state = 'open'), 0)
        + COALESCE((SELECT SUM(estimated_amount) FROM billing.admission_operations
            WHERE merchant_id = merchant AND payer_id = payer AND currency = unit AND state = 'open'
              AND (expires_at IS NULL OR expires_at > as_of)), 0)
    )::bigint;
$$;
REVOKE ALL ON FUNCTION billing.financial_held_amount(uuid, uuid, text, timestamptz) FROM PUBLIC;

-- Admission operation reservations.
CREATE TABLE billing.admission_operations (
    merchant_id uuid NOT NULL,
    request_id text NOT NULL CHECK (octet_length(request_id) BETWEEN 1 AND 255),
    payer_id uuid NOT NULL,
    currency text NOT NULL CONSTRAINT admission_operations_currency_shape CHECK (currency ~ '^[A-Z]{3,12}$'),
    estimated_amount bigint NOT NULL CHECK (estimated_amount >= 0),
    available_amount bigint NOT NULL CHECK (available_amount >= 0),
    terms jsonb NOT NULL CHECK (jsonb_typeof(terms) = 'object' AND octet_length(terms::text) <= 65536),
    requested_expires_at timestamptz,
    expires_at timestamptz,
    admitted_at timestamptz NOT NULL,
    window_keys text[] NOT NULL,
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'released', 'captured')),
    capture_terms jsonb CHECK (jsonb_typeof(capture_terms) = 'object' AND octet_length(capture_terms::text) <= 65536),
    captured_amount bigint,
    captured_at timestamptz,
    released_at timestamptz,
    PRIMARY KEY (merchant_id, request_id),
    FOREIGN KEY (merchant_id, payer_id) REFERENCES billing.customers (merchant_id, id),
    CHECK (estimated_amount = 0 OR (requested_expires_at IS NOT NULL AND requested_expires_at > admitted_at)),
    CHECK (requested_expires_at IS NULL OR (expires_at IS NOT NULL AND expires_at >= requested_expires_at)),
    CHECK (
        (state = 'open' AND capture_terms IS NULL AND captured_amount IS NULL AND captured_at IS NULL AND released_at IS NULL)
        OR (state = 'released' AND capture_terms IS NULL AND captured_amount IS NULL AND captured_at IS NULL AND released_at IS NOT NULL)
        OR (state = 'captured' AND capture_terms IS NOT NULL AND captured_amount IS NOT NULL AND captured_amount >= 0 AND captured_at IS NOT NULL)
    )
);

CREATE INDEX admission_operations_held ON billing.admission_operations (merchant_id, payer_id, currency, expires_at)
    WHERE state = 'open';
CREATE INDEX admission_operations_windows ON billing.admission_operations (merchant_id, payer_id, currency, admitted_at)
    WHERE state <> 'released';
CREATE INDEX admission_operations_window_keys ON billing.admission_operations USING gin (window_keys)
    WHERE state <> 'released';

CREATE TRIGGER immutable_admission_operation_facts BEFORE UPDATE OR DELETE ON billing.admission_operations
FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('expires_at','state','capture_terms','captured_amount','captured_at','released_at');

CREATE TABLE billing.admission_denials_hourly (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    denial_reason text NOT NULL,
    hour_at timestamp with time zone NOT NULL,
    denials bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_adh_denials_positive CHECK ((denials > 0)),
    CONSTRAINT chk_adh_hour_aligned CHECK ((hour_at = date_trunc('hour'::text, hour_at)))
);
COMMENT ON TABLE billing.admission_denials_hourly IS '#733 hourly admission-denial aggregates (merchant x payer x reason), flushed periodically from Redis counters — the hot path never writes PG per-request.';

ALTER TABLE ONLY billing.admission_denials_hourly
    ADD CONSTRAINT admission_denials_hourly_pkey PRIMARY KEY (merchant_id, customer_id, denial_reason, hour_at);

CREATE INDEX idx_adh_merchant_hour ON billing.admission_denials_hourly USING btree (merchant_id, hour_at);

ALTER TABLE ONLY billing.admission_denials_hourly
    ADD CONSTRAINT adh_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TABLE billing.operation_authorizations (
    operation_id text NOT NULL,
    merchant_id uuid NOT NULL,
    payer_id uuid NOT NULL,
    record_owner text NOT NULL,
    ledger_account_id uuid NOT NULL,
    authorized_usd_micros bigint NOT NULL,
    claim_reference text NOT NULL,
    authorization_body_bytes bytea NOT NULL,
    authorization_body_digest bytea NOT NULL,
    state text DEFAULT 'open'::text NOT NULL,
    terminal_reference text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    released_at timestamp with time zone,
    settled_at timestamp with time zone,
    settlement_provider_cost_usd_micros bigint,
    settlement_rated_usd_micros bigint,
    settlement_body_bytes bytea,
    settlement_body_digest bytea,
    CONSTRAINT operation_authorizations_amount_positive CHECK ((authorized_usd_micros > 0)),
    CONSTRAINT operation_authorizations_body_present CHECK ((octet_length(authorization_body_bytes) > 0)),
    CONSTRAINT operation_authorizations_body_size CHECK ((octet_length(authorization_body_bytes) <= 65536)),
    CONSTRAINT operation_authorizations_claim_reference_present CHECK (((claim_reference <> ''::text) AND (claim_reference = btrim(claim_reference)))),
    CONSTRAINT operation_authorizations_claim_reference_size CHECK ((octet_length(claim_reference) <= 1024)),
    CONSTRAINT operation_authorizations_digest_matches_body CHECK ((authorization_body_digest = public.digest(authorization_body_bytes, 'sha256'::text))),
    CONSTRAINT operation_authorizations_digest_shape CHECK ((octet_length(authorization_body_digest) = 32)),
    CONSTRAINT operation_authorizations_operation_id_present CHECK (((operation_id <> ''::text) AND (operation_id = btrim(operation_id)))),
    CONSTRAINT operation_authorizations_operation_id_size CHECK ((octet_length(operation_id) <= 255)),
    CONSTRAINT operation_authorizations_record_owner_present CHECK (((record_owner <> ''::text) AND (record_owner = btrim(record_owner)))),
    CONSTRAINT operation_authorizations_record_owner_size CHECK ((octet_length(record_owner) <= 255)),
    CONSTRAINT operation_authorizations_settlement_shape CHECK ((((state <> 'settled'::text) AND (settlement_provider_cost_usd_micros IS NULL) AND (settlement_rated_usd_micros IS NULL) AND (settlement_body_bytes IS NULL) AND (settlement_body_digest IS NULL)) OR ((state = 'settled'::text) AND (settlement_provider_cost_usd_micros IS NOT NULL) AND (settlement_provider_cost_usd_micros >= 0) AND (settlement_rated_usd_micros IS NOT NULL) AND (settlement_rated_usd_micros >= 0) AND (settlement_rated_usd_micros = settlement_provider_cost_usd_micros) AND (settlement_body_bytes IS NOT NULL) AND ((octet_length(settlement_body_bytes) >= 1) AND (octet_length(settlement_body_bytes) <= 65536)) AND (settlement_body_digest IS NOT NULL) AND (octet_length(settlement_body_digest) = 32) AND (settlement_body_digest = public.digest(settlement_body_bytes, 'sha256'::text)) AND (terminal_reference = ('sha256:'::text || encode(settlement_body_digest, 'hex'::text)))))),
    CONSTRAINT operation_authorizations_state_check CHECK ((state = ANY (ARRAY['open'::text, 'released'::text, 'settled'::text]))),
    CONSTRAINT operation_authorizations_terminal_reference_size CHECK (((terminal_reference IS NULL) OR (octet_length(terminal_reference) <= 1024))),
    CONSTRAINT operation_authorizations_terminal_shape CHECK ((((state = 'open'::text) AND (terminal_reference IS NULL) AND (released_at IS NULL) AND (settled_at IS NULL)) OR ((state = 'released'::text) AND (terminal_reference <> ''::text) AND (released_at IS NOT NULL) AND (settled_at IS NULL)) OR ((state = 'settled'::text) AND (terminal_reference <> ''::text) AND (released_at IS NULL) AND (settled_at IS NOT NULL))))
);
COMMENT ON TABLE billing.operation_authorizations IS 'Merchant-scoped durable th-005 financial reservations for exact provider-operation bodies. Open rows reserve USD-micro capacity against the linked customer_balance ledger account; they are not ledger movements and never TTL-expire.';
COMMENT ON COLUMN billing.operation_authorizations.authorization_body_bytes IS 'Exact canonical bytes authored by the embedding host. OpenRails binds them byte-for-byte but does not interpret their format.';
COMMENT ON COLUMN billing.operation_authorizations.authorization_body_digest IS 'Caller-bound SHA-256 of authorization_body_bytes, also rechecked by the database.';
COMMENT ON COLUMN billing.operation_authorizations.settlement_provider_cost_usd_micros IS 'Qualified final provider-cost basis supplied by the OpenRails evidence qualifier.';
COMMENT ON COLUMN billing.operation_authorizations.settlement_rated_usd_micros IS 'OpenRails-owned final customer settlement. The permanent pass-through contract maps qualified provider cost directly, so this equals settlement_provider_cost_usd_micros; it may exceed authorization and is never clamped.';
COMMENT ON COLUMN billing.operation_authorizations.settlement_body_bytes IS 'Exact canonical bytes authored by the OpenRails evidence qualifier from provider observations and lifecycle evidence.';
COMMENT ON COLUMN billing.operation_authorizations.settlement_body_digest IS 'OpenRails-derived SHA-256 of settlement_body_bytes, also rechecked by the database and used as the canonical terminal reference.';

ALTER TABLE ONLY billing.operation_authorizations
    ADD CONSTRAINT operation_authorizations_pkey PRIMARY KEY (merchant_id, operation_id);

CREATE INDEX idx_operation_authorizations_open_capacity ON billing.operation_authorizations USING btree (merchant_id, ledger_account_id) WHERE (state = 'open'::text);
CREATE INDEX idx_operation_authorizations_payer ON billing.operation_authorizations USING btree (merchant_id, payer_id, created_at DESC);

ALTER TABLE ONLY billing.operation_authorizations
    ADD CONSTRAINT operation_authorizations_ledger_account_fk FOREIGN KEY (merchant_id, payer_id, ledger_account_id) REFERENCES billing.ledger_accounts(merchant_id, customer_id, id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.operation_authorizations
    ADD CONSTRAINT operation_authorizations_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.operation_authorizations
    ADD CONSTRAINT operation_authorizations_payer_fk FOREIGN KEY (merchant_id, payer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE RESTRICT;

CREATE TRIGGER immutable_operation_authorization_facts BEFORE UPDATE OR DELETE ON billing.operation_authorizations
FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('state','terminal_reference','released_at','settled_at','settlement_provider_cost_usd_micros','settlement_rated_usd_micros','settlement_body_bytes','settlement_body_digest');

CREATE TABLE billing.provider_billing_qualifications (
    merchant_id uuid NOT NULL,
    operation_id text NOT NULL,
    provider text NOT NULL,
    provider_resource_id text NOT NULL,
    provider_lifetime_start timestamp with time zone CONSTRAINT provider_billing_qualification_provider_lifetime_start_not_null NOT NULL,
    provider_lifetime_end timestamp with time zone NOT NULL,
    provider_absent_at timestamp with time zone NOT NULL,
    provider_absence_reference text CONSTRAINT provider_billing_qualificat_provider_absence_reference_not_null NOT NULL,
    billing_stop_reference text NOT NULL,
    windows_closed_at timestamp with time zone NOT NULL,
    windows_closed_reference text CONSTRAINT provider_billing_qualificatio_windows_closed_reference_not_null NOT NULL,
    lifecycle_evidence_bytes bytea CONSTRAINT provider_billing_qualificatio_lifecycle_evidence_bytes_not_null NOT NULL,
    lifecycle_evidence_digest bytea CONSTRAINT provider_billing_qualificati_lifecycle_evidence_digest_not_null NOT NULL,
    quiescence_seconds bigint NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    reason text DEFAULT 'awaiting_equal_observation'::text NOT NULL,
    baseline_observation_id text,
    qualified_observation_id text,
    qualified_provider_cost_usd_micros bigint,
    qualified_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT provider_billing_qualification_evidence_shape CHECK ((((octet_length(lifecycle_evidence_bytes) >= 1) AND (octet_length(lifecycle_evidence_bytes) <= 65536)) AND (octet_length(lifecycle_evidence_digest) = 32) AND (lifecycle_evidence_digest = public.digest(lifecycle_evidence_bytes, 'sha256'::text)))),
    CONSTRAINT provider_billing_qualification_lifetime_shape CHECK (((provider_lifetime_end > provider_lifetime_start) AND (provider_absent_at >= provider_lifetime_end) AND (windows_closed_at >= provider_lifetime_end))),
    CONSTRAINT provider_billing_qualification_policy_shape CHECK ((quiescence_seconds > 0)),
    CONSTRAINT provider_billing_qualification_provider_shape CHECK (((provider <> ''::text) AND (provider = btrim(provider)) AND (octet_length(provider) <= 255) AND (provider_resource_id <> ''::text) AND (provider_resource_id = btrim(provider_resource_id)) AND (octet_length(provider_resource_id) <= 255))),
    CONSTRAINT provider_billing_qualification_reference_shape CHECK (((provider_absence_reference <> ''::text) AND (provider_absence_reference = btrim(provider_absence_reference)) AND (octet_length(provider_absence_reference) <= 1024) AND (billing_stop_reference <> ''::text) AND (billing_stop_reference = btrim(billing_stop_reference)) AND (octet_length(billing_stop_reference) <= 1024) AND (windows_closed_reference <> ''::text) AND (windows_closed_reference = btrim(windows_closed_reference)) AND (octet_length(windows_closed_reference) <= 1024))),
    CONSTRAINT provider_billing_qualification_state_shape CHECK (((state = ANY (ARRAY['pending'::text, 'refused'::text, 'eligible'::text])) AND (reason = ANY (ARRAY['awaiting_equal_observation'::text, 'awaiting_quiescence'::text, 'coverage_incomplete'::text, 'observation_changed'::text, 'provider_evidence_refused'::text, 'negative_or_corrective_record'::text, 'decreasing_provider_cost'::text, 'eligible'::text])) AND ((baseline_observation_id IS NULL) OR ((baseline_observation_id <> ''::text) AND (baseline_observation_id = btrim(baseline_observation_id)) AND (octet_length(baseline_observation_id) <= 255))) AND ((qualified_observation_id IS NULL) OR ((qualified_observation_id <> ''::text) AND (qualified_observation_id = btrim(qualified_observation_id)) AND (octet_length(qualified_observation_id) <= 255))) AND (((state = 'pending'::text) AND (reason = ANY (ARRAY['awaiting_equal_observation'::text, 'awaiting_quiescence'::text, 'coverage_incomplete'::text, 'observation_changed'::text])) AND (qualified_observation_id IS NULL) AND (qualified_provider_cost_usd_micros IS NULL) AND (qualified_at IS NULL)) OR ((state = 'refused'::text) AND (reason = ANY (ARRAY['provider_evidence_refused'::text, 'negative_or_corrective_record'::text, 'decreasing_provider_cost'::text])) AND (qualified_observation_id IS NULL) AND (qualified_provider_cost_usd_micros IS NULL) AND (qualified_at IS NULL)) OR ((state = 'eligible'::text) AND (reason = 'eligible'::text) AND (baseline_observation_id IS NOT NULL) AND (qualified_observation_id IS NOT NULL) AND (qualified_provider_cost_usd_micros IS NOT NULL) AND (qualified_provider_cost_usd_micros >= 0) AND (qualified_at IS NOT NULL)))))
);
COMMENT ON TABLE billing.provider_billing_qualifications IS 'OpenRails-owned th-045 post-absence qualification state for one operation authorization. Eligible is an operator quiescence policy fact, never provider-attested finality.';

ALTER TABLE ONLY billing.provider_billing_qualifications
    ADD CONSTRAINT provider_billing_qualifications_pkey PRIMARY KEY (merchant_id, operation_id);

ALTER TABLE ONLY billing.provider_billing_qualifications
    ADD CONSTRAINT provider_billing_qualification_operation_fk FOREIGN KEY (merchant_id, operation_id) REFERENCES billing.operation_authorizations(merchant_id, operation_id) ON DELETE RESTRICT;

CREATE TRIGGER immutable_provider_qualification_facts BEFORE UPDATE OR DELETE ON billing.provider_billing_qualifications
FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('state','reason','baseline_observation_id','qualified_observation_id','qualified_provider_cost_usd_micros','qualified_at','updated_at');

CREATE TABLE billing.provider_billing_observations (
    merchant_id uuid NOT NULL,
    operation_id text NOT NULL,
    observation_id text NOT NULL,
    normalized_query text NOT NULL,
    query_start timestamp with time zone NOT NULL,
    query_end timestamp with time zone NOT NULL,
    raw_body_available boolean NOT NULL,
    raw_body_bytes bytea NOT NULL,
    raw_body_digest bytea NOT NULL,
    normalized_records_bytes bytea,
    normalized_records_digest bytea,
    provider_cost_usd_micros bigint,
    has_negative_record boolean DEFAULT false NOT NULL,
    refusal_kind text,
    covers_lifetime boolean NOT NULL,
    qualification_reason text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    CONSTRAINT provider_billing_observation_id_shape CHECK (((observation_id <> ''::text) AND (observation_id = btrim(observation_id)) AND (octet_length(observation_id) <= 255))),
    CONSTRAINT provider_billing_observation_normalized_shape CHECK ((((refusal_kind IS NULL) AND raw_body_available AND (octet_length(raw_body_bytes) > 0) AND (normalized_records_bytes IS NOT NULL) AND (octet_length(normalized_records_bytes) > 0) AND (octet_length(normalized_records_bytes) <= 786432) AND (normalized_records_digest IS NOT NULL) AND (octet_length(normalized_records_digest) = 32) AND (normalized_records_digest = public.digest(normalized_records_bytes, 'sha256'::text)) AND (provider_cost_usd_micros IS NOT NULL)) OR ((refusal_kind IS NOT NULL) AND (refusal_kind <> ''::text) AND (refusal_kind = btrim(refusal_kind)) AND (octet_length(refusal_kind) <= 255) AND (normalized_records_bytes IS NULL) AND (normalized_records_digest IS NULL) AND (provider_cost_usd_micros IS NULL) AND (NOT has_negative_record) AND (NOT covers_lifetime) AND (qualification_reason = 'provider_evidence_refused'::text) AND (((refusal_kind = ANY (ARRAY['schema_ambiguity'::text, 'submicro_amount'::text, 'amount_overflow'::text])) AND raw_body_available AND (octet_length(raw_body_bytes) > 0)) OR ((refusal_kind = 'response_too_large'::text) AND (NOT raw_body_available) AND (octet_length(raw_body_bytes) = 0)))))),
    CONSTRAINT provider_billing_observation_query_shape CHECK (((normalized_query <> ''::text) AND (normalized_query = btrim(normalized_query)) AND (octet_length(normalized_query) <= 8192) AND (query_end > query_start))),
    CONSTRAINT provider_billing_observation_raw_shape CHECK (((octet_length(raw_body_bytes) <= 786432) AND (octet_length(raw_body_digest) = 32) AND (raw_body_digest = public.digest(raw_body_bytes, 'sha256'::text)) AND (raw_body_available OR (octet_length(raw_body_bytes) = 0)))),
    CONSTRAINT provider_billing_observation_reason_shape CHECK ((qualification_reason = ANY (ARRAY['awaiting_equal_observation'::text, 'awaiting_quiescence'::text, 'coverage_incomplete'::text, 'observation_changed'::text, 'provider_evidence_refused'::text, 'negative_or_corrective_record'::text, 'decreasing_provider_cost'::text, 'eligible'::text])))
);
COMMENT ON TABLE billing.provider_billing_observations IS 'Append-only provider-neutral billing reads. Exact bounded raw bodies and OpenRails-canonical normalized records remain evidence; no row is a ledger movement.';

ALTER TABLE ONLY billing.provider_billing_observations
    ADD CONSTRAINT provider_billing_observations_pkey PRIMARY KEY (merchant_id, operation_id, observation_id);

CREATE INDEX idx_provider_billing_observations_operation_time ON billing.provider_billing_observations USING btree (merchant_id, operation_id, observed_at DESC);

ALTER TABLE ONLY billing.provider_billing_observations
    ADD CONSTRAINT provider_billing_observation_qualification_fk FOREIGN KEY (merchant_id, operation_id) REFERENCES billing.provider_billing_qualifications(merchant_id, operation_id) ON DELETE RESTRICT;

CREATE TRIGGER immutable_provider_billing_observations BEFORE UPDATE OR DELETE ON billing.provider_billing_observations
FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- ---------------------------------------------------------------------------
-- Provider operations
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.guard_provider_cutover_intent() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.intent_type IN ('nmi_vault_delete','nmi_payment_method_update') THEN
   PERFORM 1 FROM billing.payment_methods WHERE merchant_id=NEW.merchant_id AND id=(NEW.payload->>'payment_method_id')::uuid FOR UPDATE;
   IF EXISTS(SELECT 1 FROM billing.rail_intents i WHERE i.merchant_id=NEW.merchant_id AND i.intent_type='nmi_provider_cutover'
      AND i.status NOT IN ('succeeded','failed_terminal','superseded','expired')
      AND (i.payload->'request'->>'target_payment_method_id'=NEW.payload->>'payment_method_id' OR i.payload->>'source_payment_method_id'=NEW.payload->>'payment_method_id')) THEN
     RAISE EXCEPTION 'payment method has an unresolved provider cutover' USING ERRCODE='55000';
   END IF;
 END IF;
 IF NEW.subscription_id IS NOT NULL THEN
   PERFORM 1 FROM billing.subscriptions WHERE id=NEW.subscription_id AND merchant_id=NEW.merchant_id FOR UPDATE;
   IF EXISTS(SELECT 1 FROM billing.rail_intents i WHERE i.merchant_id=NEW.merchant_id
     AND i.subscription_id=NEW.subscription_id AND i.id<>NEW.id
     AND i.idempotency_key<>NEW.idempotency_key
     AND i.status NOT IN ('succeeded','failed_terminal','superseded','expired')
     AND (i.intent_type='nmi_provider_cutover' OR NEW.intent_type='nmi_provider_cutover')) THEN
     RAISE EXCEPTION 'subscription has an unresolved provider operation' USING ERRCODE='55000';
   END IF;
 END IF;
 RETURN NEW;
END $$;

CREATE TABLE billing.rail_intents (
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
    psp_id uuid,
    destructive_run_id uuid,
    destructive_run_class text GENERATED ALWAYS AS (CASE WHEN destructive_run_id IS NOT NULL THEN 'destructive' END) STORED,
    custodian_id uuid,
    CONSTRAINT chk_rail_intents_executed CHECK (((status <> 'succeeded'::text) OR (executed_at IS NOT NULL))),
    CONSTRAINT chk_rail_intents_origin CHECK ((origin = ANY (ARRAY['user'::text, 'admin'::text, 'system'::text]))),
    CONSTRAINT chk_rail_intents_status CHECK ((status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'succeeded'::text, 'unknown_needs_verify'::text, 'failed_retryable'::text, 'failed_terminal'::text, 'superseded'::text, 'expired'::text]))),
    CONSTRAINT rail_intents_addressed CHECK (((psp_id IS NOT NULL) OR (custodian_id IS NOT NULL)))
);
COMMENT ON TABLE billing.rail_intents IS 'Durable, effectively-once outbox for outbound provider mutations (#358). One row per logical intent (unique per tenant on idempotency_key); the executor worker drains whatever is currently executable, the verifier resolves ambiguous outcomes via provider reads.';
COMMENT ON COLUMN billing.rail_intents.rail IS 'Rail the mutation targets (e.g. ''nmi'', ''stripe'').';
COMMENT ON COLUMN billing.rail_intents.intent_type IS 'Registry key selecting the per-type semantics (executor, verifier, relevance, backoff): nmi_delete_subscription, and in later phases manual_rebill, refund, plan_archive, ...';
COMMENT ON COLUMN billing.rail_intents.idempotency_key IS 'Deterministic identity of the logical intent within the tenant. Re-enqueues conflict here: a pending intent is refreshed, a superseded/expired one revived (relevance returned), anything else untouched — effectively-once per logical intent.';
COMMENT ON COLUMN billing.rail_intents.claimed_until IS 'Single-executor lease (SKIP LOCKED claim). An in_flight row whose lease elapsed was orphaned by a crashed executor and becomes claimable again; per-type execute semantics (verify-then-execute, verifier-before-retry) make the reclaim safe.';
COMMENT ON COLUMN billing.rail_intents.origin IS 'Who wanted this mutation: user/admin-origin intents execute under mode=limited (reactive completion), system-origin intents require mode=full. Nothing executes under mode=readonly.';
COMMENT ON COLUMN billing.rail_intents.actor IS 'Authenticated principal id (admin user id or self-service customer id) that produced a user/admin-origin intent. NULL for system-origin. Powers the #732 anti-credential-compromise rate ceiling (per-actor + per-merchant rolling-hour count of destructive ops).';
COMMENT ON COLUMN billing.rail_intents.last_failure_reason IS 'Why the most recent attempt did not succeed (mode parked, kill switch, provider down, declined...). Recorded on the intent, never surfaced as an error.';
COMMENT ON COLUMN billing.rail_intents.expires_at IS 'End of the relevance window: past this instant the intent expires with a finding instead of firing stale (NULL = relevance governed solely by the type''s relevance check).';
COMMENT ON COLUMN billing.rail_intents.result_evidence IS 'How the terminal status was established (e.g. {"verified_absent": true} for a delete confirmed by a provider read).';
COMMENT ON COLUMN billing.rail_intents.psp_id IS 'PSP the outbound intent was enqueued against. Required unless the intent is custodian-addressed (rail_intents_addressed) — or#893/or#795.';
COMMENT ON COLUMN billing.rail_intents.destructive_run_id IS 'or#859: the destructive run whose pass enqueued this intent. The reverse of that run supersedes the ones still pending/failed_retryable and reports the rest — succeeded ones as irreversible provider-side divergence, in_flight/unknown_needs_verify ones as ambiguous. Attribution only: never cleared, never used to delete a row.';
COMMENT ON COLUMN billing.rail_intents.custodian_id IS 'or#893/or#795: the custodian this outbound write is addressed to, for intents that target a custodian rather than a gateway account (the batch account updater). NULL for the ordinary PSP-addressed intent. Composite FK: an intent can only reference ITS OWN merchant''s custodian.';

ALTER TABLE ONLY billing.rail_intents
    ADD CONSTRAINT rail_intents_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.rail_intents
    ADD CONSTRAINT rail_intents_merchant_id_id_key UNIQUE (merchant_id, id);

CREATE INDEX idx_rail_intents_actor_created ON billing.rail_intents USING btree (actor, created_at) WHERE (actor IS NOT NULL);
CREATE INDEX idx_rail_intents_created ON billing.rail_intents USING btree (created_at);
CREATE INDEX idx_rail_intents_custodian ON billing.rail_intents USING btree (custodian_id) WHERE (custodian_id IS NOT NULL);
-- Exact handle lookup serves both pending exclusion and permanent erasure history.
CREATE INDEX idx_rail_intents_custodian_method_delete ON billing.rail_intents
    (merchant_id, custodian_id, (payload->'instrument'->>'rail_method_ref'))
    WHERE intent_type='hyperswitch_method_delete';
CREATE INDEX idx_rail_intents_native_vault_delete ON billing.rail_intents
    (merchant_id, psp_id, (payload->>'rail_customer_ref'))
    WHERE intent_type='nmi_vault_delete';
CREATE INDEX idx_rail_intents_destructive_actor_window ON billing.rail_intents USING btree (actor, created_at, intent_type) WHERE (origin = ANY (ARRAY['user'::text, 'admin'::text]));
CREATE INDEX idx_rail_intents_destructive_run ON billing.rail_intents USING btree (destructive_run_id) WHERE (destructive_run_id IS NOT NULL);
CREATE INDEX idx_rail_intents_due ON billing.rail_intents USING btree (next_attempt_at) WHERE (status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'failed_retryable'::text, 'unknown_needs_verify'::text]));
CREATE INDEX idx_rail_intents_merchant_destructive_window ON billing.rail_intents USING btree (merchant_id, origin, created_at, intent_type);
CREATE INDEX idx_rail_intents_merchant_id ON billing.rail_intents USING btree (merchant_id);
CREATE INDEX idx_rail_intents_psp ON billing.rail_intents USING btree (psp_id);
CREATE INDEX idx_rail_intents_subscription ON billing.rail_intents USING btree (subscription_id) WHERE (subscription_id IS NOT NULL);
-- Initial membership identity is frozen in terms, including failed attempts
-- with no subscription row. Paid agreement lookup must not scan the whole book.
CREATE INDEX idx_rail_intents_initial_membership_history ON billing.rail_intents
    (merchant_id, ((payload->'terms')->>'subscription_id'))
    WHERE intent_type='initial_membership' AND status='succeeded';
CREATE UNIQUE INDEX uq_rail_intents_merchant_idempotency_key ON billing.rail_intents USING btree (merchant_id, idempotency_key);
CREATE UNIQUE INDEX uq_rail_intents_open_subscription_collection ON billing.rail_intents (merchant_id, subscription_id)
WHERE intent_type = 'subscription_collection' AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable');
CREATE UNIQUE INDEX uq_rail_intents_subscription_collection_slot
ON billing.rail_intents (merchant_id, subscription_id, (payload->>'previous_period_end'), (payload->>'attempt'))
WHERE intent_type = 'subscription_collection';
CREATE UNIQUE INDEX uq_rail_intents_open_manual_rebill ON billing.rail_intents (merchant_id, subscription_id)
WHERE intent_type = 'manual_rebill' AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable');
CREATE UNIQUE INDEX uq_rail_intents_tier_change_subscription ON billing.rail_intents(merchant_id, subscription_id)
WHERE intent_type IN ('nmi_upgrade', 'stripe_tier_change', 'initial_membership')
  AND subscription_id IS NOT NULL
  AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable');

ALTER TABLE ONLY billing.rail_intents
    ADD CONSTRAINT rail_intents_custodian_fk FOREIGN KEY (custodian_id, merchant_id) REFERENCES billing.custodians(id, merchant_id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.rail_intents
    ADD CONSTRAINT rail_intents_destructive_run_fk FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs(merchant_id, id, run_class) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.rail_intents
    ADD CONSTRAINT rail_intents_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.rail_intents
    ADD CONSTRAINT rail_intents_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id);

CREATE TRIGGER guard_provider_cutover_intent BEFORE INSERT OR UPDATE OF status ON billing.rail_intents
FOR EACH ROW EXECUTE FUNCTION billing.guard_provider_cutover_intent();

CREATE TABLE billing.rail_mutation_logs (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    psp_id uuid,
    rail_intent_id uuid,
    intent_type text,
    idempotency_key text,
    attempt integer DEFAULT 0 NOT NULL,
    phase text NOT NULL,
    reason text,
    evidence jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    custodian_id uuid,
    CONSTRAINT rail_mutation_logs_addressed CHECK (((psp_id IS NOT NULL) OR (custodian_id IS NOT NULL))),
    CONSTRAINT rail_mutation_logs_phase_check CHECK ((phase = ANY (ARRAY['attempting'::text, 'succeeded'::text, 'failed'::text, 'unknown'::text, 'parked'::text])))
);
COMMENT ON TABLE billing.rail_mutation_logs IS 'Append-only operator history for external provider mutations executed from provider intents/convergence (#533). or#859 Class A: the record of what we did to the outside world — INSERT plus the whole-merchant purge DELETE only, never UPDATE, and never rolled back.';
COMMENT ON COLUMN billing.rail_mutation_logs.psp_id IS 'PSP the logged mutation was addressed to. Required unless the mutation is custodian-addressed (rail_mutation_logs_addressed) — or#893/or#795.';
COMMENT ON COLUMN billing.rail_mutation_logs.phase IS 'Provider mutation lifecycle phase: attempting before the remote call, then succeeded/failed/unknown/parked after the handler classifies the result.';
COMMENT ON COLUMN billing.rail_mutation_logs.evidence IS 'Scrubbed structured metadata only. Never store API keys, authorization headers, card data, private keys, or unsanitized provider bodies.';
COMMENT ON COLUMN billing.rail_mutation_logs.custodian_id IS 'or#893/or#795: the custodian the logged mutation was addressed to, for custodian-addressed intents. NULL for the ordinary PSP-addressed mutation.';

ALTER TABLE ONLY billing.rail_mutation_logs
    ADD CONSTRAINT rail_mutation_logs_pkey PRIMARY KEY (id);

CREATE INDEX idx_rail_mutation_logs_custodian ON billing.rail_mutation_logs USING btree (custodian_id) WHERE (custodian_id IS NOT NULL);
CREATE INDEX idx_rail_mutation_logs_merchant_created ON billing.rail_mutation_logs USING btree (merchant_id, created_at DESC);
CREATE INDEX idx_rail_mutation_logs_psp ON billing.rail_mutation_logs USING btree (psp_id);
CREATE INDEX idx_rail_mutation_logs_rail_intent ON billing.rail_mutation_logs USING btree (rail_intent_id) WHERE (rail_intent_id IS NOT NULL);
CREATE INDEX idx_rail_mutation_logs_rail_phase ON billing.rail_mutation_logs USING btree (rail, phase, created_at DESC);

ALTER TABLE ONLY billing.rail_mutation_logs
    ADD CONSTRAINT rail_mutation_logs_custodian_fk FOREIGN KEY (custodian_id, merchant_id) REFERENCES billing.custodians(id, merchant_id) ON DELETE CASCADE;
ALTER TABLE ONLY billing.rail_mutation_logs
    ADD CONSTRAINT rail_mutation_logs_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE;
ALTER TABLE ONLY billing.rail_mutation_logs
    ADD CONSTRAINT rail_mutation_logs_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id) ON DELETE CASCADE;
ALTER TABLE ONLY billing.rail_mutation_logs
    ADD CONSTRAINT rail_mutation_logs_rail_intent_fk FOREIGN KEY (merchant_id, rail_intent_id) REFERENCES billing.rail_intents(merchant_id, id) ON DELETE SET NULL (rail_intent_id);

CREATE TRIGGER immutable_rail_mutation_log_content BEFORE UPDATE ON billing.rail_mutation_logs
FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.rail_refresh_watermarks (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    psp_id uuid NOT NULL,
    event_domain text NOT NULL,
    watermark_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT rail_refresh_watermarks_event_domain_check CHECK ((event_domain = ANY (ARRAY['events'::text]))),
    CONSTRAINT rail_refresh_watermarks_rail_check CHECK ((rail = ANY (ARRAY['nmi'::text, 'ccbill'::text, 'stripe'::text, 'solana'::text])))
);
COMMENT ON TABLE billing.rail_refresh_watermarks IS 'Durable Provider Refresh watermarks: the exclusive lower bound for the next bounded event window, per (merchant, rail, PSP, domain). A failed or partial provider read simply never advances watermark_at — the failure itself is recorded by the job, not here.';
COMMENT ON COLUMN billing.rail_refresh_watermarks.psp_id IS 'The PSP whose event stream this cursor bounds. Required (or#893): a pull arms from exactly one PSP, and a watermark shared across PSPs skips the events of every PSP but the one that advanced it.';
COMMENT ON COLUMN billing.rail_refresh_watermarks.event_domain IS 'Refresh domain. events currently covers provider transaction/subscription event windows.';
COMMENT ON COLUMN billing.rail_refresh_watermarks.watermark_at IS 'Exclusive lower bound for the next successful bounded provider event refresh window.';

ALTER TABLE ONLY billing.rail_refresh_watermarks
    ADD CONSTRAINT rail_refresh_watermarks_identity_key UNIQUE (merchant_id, rail, psp_id, event_domain);
ALTER TABLE ONLY billing.rail_refresh_watermarks
    ADD CONSTRAINT rail_refresh_watermarks_pkey PRIMARY KEY (id);

CREATE INDEX idx_rail_refresh_watermarks_psp ON billing.rail_refresh_watermarks USING btree (psp_id);
CREATE INDEX idx_rail_refresh_watermarks_rail ON billing.rail_refresh_watermarks USING btree (rail, event_domain, watermark_at);

ALTER TABLE ONLY billing.rail_refresh_watermarks
    ADD CONSTRAINT rail_refresh_watermarks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE;
ALTER TABLE ONLY billing.rail_refresh_watermarks
    ADD CONSTRAINT rail_refresh_watermarks_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id) ON DELETE CASCADE;

CREATE TABLE billing.webhook_events (
    merchant_id uuid NOT NULL,
    op text NOT NULL,
    event_id text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    completed_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);
COMMENT ON TABLE billing.webhook_events IS '#678 webhook dedup truth: one row per applied webhook event (merchant, op, event_id). Pending/lease state stays in Redis (coordination, not truth); a row here means effects are durably applied.';
COMMENT ON COLUMN billing.webhook_events.op IS 'Dedup operation key, webhook.<rail>.<event_type> — matches the Redis key derivation.';

ALTER TABLE ONLY billing.webhook_events
    ADD CONSTRAINT webhook_events_pkey PRIMARY KEY (merchant_id, op, event_id);

CREATE INDEX idx_webhook_events_completed_at ON billing.webhook_events USING btree (completed_at);
CREATE INDEX ix_webhook_events_retention ON billing.webhook_events USING btree (merchant_id, completed_at);

ALTER TABLE ONLY billing.webhook_events
    ADD CONSTRAINT webhook_events_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TRIGGER immutable_webhook_event_content BEFORE UPDATE ON billing.webhook_events
FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.webhook_health (
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    last_accepted_at timestamp with time zone,
    last_pull_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT webhook_health_rail_nonempty CHECK ((btrim(rail) <> ''::text))
);
COMMENT ON TABLE billing.webhook_health IS '#786 per-(merchant, rail) inbound-webhook health: accepted/rejected/drift watermarks + counters. last_accepted_at is stamped only by signature-verified webhooks; last_pull_at is the provider-refresh pull watermark the drift gate uses.';
COMMENT ON COLUMN billing.webhook_health.last_accepted_at IS 'last signature-VERIFIED webhook for this rail; silence age is measured from here (or created_at when nothing was ever accepted).';

ALTER TABLE ONLY billing.webhook_health
    ADD CONSTRAINT webhook_health_pkey PRIMARY KEY (merchant_id, rail);

ALTER TABLE ONLY billing.webhook_health
    ADD CONSTRAINT webhook_health_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE;

CREATE TABLE billing.webhook_health_daily (
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    day_at timestamp with time zone NOT NULL,
    rejected bigint DEFAULT 0 NOT NULL,
    drift bigint DEFAULT 0 NOT NULL,
    CONSTRAINT webhook_health_daily_rail_nonempty CHECK ((btrim(rail) <> ''::text))
);
COMMENT ON TABLE billing.webhook_health_daily IS '#786 UTC-day webhook counter buckets backing the #733 webhook_rejects / webhook_drift_events windowed metrics.';

ALTER TABLE ONLY billing.webhook_health_daily
    ADD CONSTRAINT webhook_health_daily_pkey PRIMARY KEY (merchant_id, rail, day_at);

ALTER TABLE ONLY billing.webhook_health_daily
    ADD CONSTRAINT webhook_health_daily_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE;

CREATE TABLE billing.account_updater_batches (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    custodian_id uuid NOT NULL,
    job_ref text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    instruments jsonb DEFAULT '[]'::jsonb NOT NULL,
    result_counts jsonb DEFAULT '{}'::jsonb NOT NULL,
    failure_reason text DEFAULT ''::text NOT NULL,
    submitted_at timestamp with time zone,
    last_polled_at timestamp with time zone,
    completed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT account_updater_batches_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'submitted'::text, 'completed'::text, 'failed'::text]))),
    CONSTRAINT account_updater_batches_submitted_has_job CHECK (((status <> 'submitted'::text) OR (btrim(job_ref) <> ''::text)))
);
COMMENT ON TABLE billing.account_updater_batches IS 'or#795: one batch account-updater cycle for one custodian. Written BEFORE the provider is touched and kept until the results are folded, so a worker restart between submit and ingest RESUMES POLLING the recorded job instead of resubmitting a paid batch. The membership is recorded verbatim; the result vocabulary is counted verbatim.';
COMMENT ON COLUMN billing.account_updater_batches.job_ref IS 'The custodian-native job id (Basis Theory account-updater job). '''' until the create call is confirmed.';
COMMENT ON COLUMN billing.account_updater_batches.status IS 'pending = assembled, not yet confirmed at the custodian | submitted = the custodian owns it, poll for results | completed = results folded | failed = abandoned (the instruments become due again; nothing is parked on our own malfunction).';

ALTER TABLE ONLY billing.account_updater_batches
    ADD CONSTRAINT account_updater_batches_pkey PRIMARY KEY (id);

CREATE INDEX ix_account_updater_batches_merchant_status ON billing.account_updater_batches USING btree (merchant_id, status, created_at);
CREATE UNIQUE INDEX uq_account_updater_batches_job ON billing.account_updater_batches USING btree (merchant_id, custodian_id, job_ref) WHERE (job_ref <> ''::text);
CREATE UNIQUE INDEX uq_account_updater_batches_open ON billing.account_updater_batches USING btree (merchant_id, custodian_id) WHERE (status = ANY (ARRAY['pending'::text, 'submitted'::text]));

ALTER TABLE ONLY billing.account_updater_batches
    ADD CONSTRAINT account_updater_batches_custodian_fk FOREIGN KEY (custodian_id, merchant_id) REFERENCES billing.custodians(id, merchant_id) ON DELETE CASCADE;
ALTER TABLE ONLY billing.account_updater_batches
    ADD CONSTRAINT account_updater_batches_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE;

-- A bulk verification read (roster + transactions by date range) resumes from
-- its last completed transaction page after a crash.
CREATE TABLE billing.nmi_bulk_checkpoints (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    since timestamp with time zone NOT NULL,
    until timestamp with time zone NOT NULL,
    next_page integer DEFAULT 1 NOT NULL,
    started_at timestamp with time zone NOT NULL,
    CONSTRAINT nmi_bulk_checkpoints_pkey PRIMARY KEY (merchant_id, psp_id),
    CONSTRAINT nmi_bulk_checkpoints_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.nmi_bulk_checkpoints IS '#1094: the in-progress bulk verification read per NMI account: its transaction window and the next page to read. Deleted when the pass completes.';

CREATE TABLE billing.nmi_history_months (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    month timestamp with time zone NOT NULL,
    kind text NOT NULL,
    category text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    authorizations bigint NOT NULL,
    CONSTRAINT nmi_history_months_pkey PRIMARY KEY (merchant_id, psp_id, month, kind, category, reason),
    CONSTRAINT nmi_history_months_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT chk_nmi_history_months_month CHECK (month = date_trunc('month', month, 'UTC')),
    CONSTRAINT chk_nmi_history_months_kind CHECK (kind IN ('verification', 'one_off_sale', 'scheduled_rebill')),
    CONSTRAINT chk_nmi_history_months_category CHECK (category IN ('approved', 'card_data', 'issuer_soft', 'issuer_hard', 'gateway_rule', 'system_error', 'unknown')),
    CONSTRAINT chk_nmi_history_months_reason CHECK ((category = 'approved') = (reason = '')),
    CONSTRAINT chk_nmi_history_months_authorizations CHECK (authorizations > 0)
);
COMMENT ON TABLE billing.nmi_history_months IS '#1120 authorizations NMI answered per PSP, month (its first instant, UTC), kind (verification, one_off_sale, scheduled_rebill) and outcome: category approved, or a refusal''s category and reason from the one classifier. A read replaces every month it covers.';

CREATE INDEX idx_nmi_history_months_month ON billing.nmi_history_months USING btree (merchant_id, month);

-- One row per NMI PSP whose history was read: the last read that completed.
CREATE TABLE billing.nmi_history_reads (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    read_at timestamp with time zone NOT NULL,
    CONSTRAINT nmi_history_reads_pkey PRIMARY KEY (merchant_id, psp_id),
    CONSTRAINT nmi_history_reads_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.nmi_history_reads IS '#1120 when each NMI PSP''s history was last read in full. A PSP with none is backfilled 25 months; later reads start the month before this one.';

-- ---------------------------------------------------------------------------
-- Invoicing
-- ---------------------------------------------------------------------------

CREATE TABLE billing.invoices (
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
    po_number text,
    tax jsonb DEFAULT '{}'::jsonb NOT NULL,
    billing_contacts jsonb DEFAULT '[]'::jsonb NOT NULL,
    memo text,
    collection_failure_count integer DEFAULT 0 NOT NULL,
    collection_failed_at timestamp with time zone,
    next_collection_attempt_at timestamp with time zone,
    last_collection_failure_code text,
    last_collection_failure_message text,
    collection_intent_id uuid,
    CONSTRAINT invoices_amounts_nonneg_chk CHECK (((subtotal_amount >= 0) AND (total_amount >= 0) AND (amount_paid >= 0) AND (amount_due >= 0))),
    CONSTRAINT invoices_collection_failure_count_nonneg CHECK ((collection_failure_count >= 0)),
    CONSTRAINT invoices_collection_method_check CHECK ((collection_method = ANY (ARRAY['charge_automatically'::text, 'send_invoice'::text]))),
    CONSTRAINT invoices_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text))),
    CONSTRAINT invoices_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'open'::text, 'paid'::text, 'past_due'::text, 'voided'::text, 'uncollectible'::text, 'finalized'::text])))
);
COMMENT ON TABLE billing.invoices IS 'Period invoices/statements. For arrears, an open invoice is the receivable and payments are allocated to it. Prepaid invoices remain informational receipts/statements.';
COMMENT ON COLUMN billing.invoices.amount_due IS 'Outstanding amount for this invoice in the row currency internal precision. Open arrears balance is derived from open/past-due invoices.';
COMMENT ON COLUMN billing.invoices.line_items IS 'Immutable as-billed statement itemization frozen at close (#726): per-event_type usage rollups. The only reader-facing line-item representation.';
COMMENT ON COLUMN billing.invoices.po_number IS '#798 purchase-order reference snapshotted from the payer invoice profile at finalize.';
COMMENT ON COLUMN billing.invoices.tax IS '#798 tax document fields (tax id, jurisdiction, rates) snapshotted from the payer invoice profile at finalize. Host-defined shape.';
COMMENT ON COLUMN billing.invoices.billing_contacts IS '#798 billing contacts ([{name,email}]) snapshotted from the payer invoice profile at finalize.';
COMMENT ON COLUMN billing.invoices.collection_intent_id IS 'The live invoice_collection operation (rail_intents) charging this invoice. One operation at a time; set on enqueue, cleared only by that operation''s terminal outcome. Blocks competing collection, void, uncollectible and out-of-band payment while set.';

ALTER TABLE ONLY billing.invoices
    ADD CONSTRAINT invoices_pkey PRIMARY KEY (id);
ALTER TABLE ONLY billing.invoices
    ADD CONSTRAINT invoices_merchant_payer_currency_id_key UNIQUE (merchant_id, customer_id, currency, id);
ALTER TABLE ONLY billing.invoices
    ADD CONSTRAINT invoices_merchant_id_id_key UNIQUE (merchant_id, id);

CREATE INDEX idx_invoices_customer ON billing.invoices USING btree (customer_id, period_from DESC);
CREATE INDEX ix_invoices_collection_due ON billing.invoices USING btree (merchant_id, next_collection_attempt_at, due_at) WHERE ((status = ANY (ARRAY['open'::text, 'past_due'::text])) AND (amount_due > 0) AND (collection_method = 'charge_automatically'::text));
CREATE INDEX ix_invoices_merchant_status_period ON billing.invoices USING btree (merchant_id, status, period_from DESC, id DESC);
CREATE INDEX ix_invoices_open_due ON billing.invoices USING btree (merchant_id, customer_id, currency, due_at) WHERE ((status = ANY (ARRAY['open'::text, 'past_due'::text])) AND (amount_due > 0));
CREATE INDEX ix_invoices_payer ON billing.invoices USING btree (merchant_id, customer_id, period_from DESC);
CREATE UNIQUE INDEX uq_invoices_period ON billing.invoices USING btree (merchant_id, customer_id, currency, period_from, period_to);
CREATE INDEX ix_invoices_collection_intent ON billing.invoices USING btree (merchant_id, collection_intent_id) WHERE (collection_intent_id IS NOT NULL);

ALTER TABLE ONLY billing.invoices
    ADD CONSTRAINT invoices_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.invoices
    ADD CONSTRAINT invoices_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.invoices
    ADD CONSTRAINT invoices_collection_intent_fk FOREIGN KEY (merchant_id, collection_intent_id) REFERENCES billing.rail_intents(merchant_id, id) ON DELETE RESTRICT;

CREATE TABLE billing.invoice_items (
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
    CONSTRAINT invoice_items_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text))),
    CONSTRAINT invoice_items_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'invoiced'::text, 'voided'::text])))
);
COMMENT ON TABLE billing.invoice_items IS 'Pending-accrual workspace (#726): owed accruals queue as pending rows gating arrears exposure; finalization attaches them (invoice_id, status=invoiced) so they cannot bill twice. NOT the statement itemization — that is invoices.line_items.';

ALTER TABLE ONLY billing.invoice_items
    ADD CONSTRAINT invoice_items_pkey PRIMARY KEY (id);

CREATE INDEX ix_invoice_items_invoice ON billing.invoice_items USING btree (merchant_id, invoice_id);
CREATE INDEX ix_invoice_items_pending ON billing.invoice_items USING btree (merchant_id, customer_id, currency, invoice_at) WHERE ((invoice_id IS NULL) AND (status = 'pending'::text));
CREATE UNIQUE INDEX uq_invoice_items_source ON billing.invoice_items USING btree (merchant_id, customer_id, currency, source_type, source_id);

ALTER TABLE ONLY billing.invoice_items
    ADD CONSTRAINT invoice_items_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.invoice_items
    ADD CONSTRAINT invoice_items_invoice_fk FOREIGN KEY (merchant_id, customer_id, currency, invoice_id) REFERENCES billing.invoices(merchant_id, customer_id, currency, id) ON DELETE SET NULL (invoice_id);
ALTER TABLE ONLY billing.invoice_items
    ADD CONSTRAINT invoice_items_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TABLE billing.invoice_payments (
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
    psp_id uuid,
    failure_reason text,
    payment_method_id uuid,
    idempotency_key text,
    CONSTRAINT invoice_payments_amount_positive_chk CHECK ((amount > 0)),
    CONSTRAINT invoice_payments_currency_shape CHECK (((currency IS NULL) OR (currency ~ '^[A-Z0-9]{3,12}$'::text))),
    CONSTRAINT invoice_payments_psp_required_on_rail CHECK (((psp_id IS NOT NULL) OR (rail = ANY (ARRAY['manual'::text, 'admin'::text])))),
    CONSTRAINT invoice_payments_status_check CHECK ((status = ANY (ARRAY['attempted'::text, 'settled'::text, 'failed'::text])))
);
COMMENT ON TABLE billing.invoice_payments IS 'Payment attempts and settled payments allocated to a specific invoice.';
COMMENT ON COLUMN billing.invoice_payments.psp_id IS 'PSP that took this invoice payment attempt. Required on every real rail (invoice_payments_psp_required_on_rail); NULL only for off-rail manual settlement — or#893.';

ALTER TABLE ONLY billing.invoice_payments
    ADD CONSTRAINT invoice_payments_pkey PRIMARY KEY (id);

CREATE INDEX idx_invoice_payments_psp ON billing.invoice_payments USING btree (psp_id) WHERE (psp_id IS NOT NULL);
CREATE INDEX ix_invoice_payments_invoice ON billing.invoice_payments USING btree (merchant_id, invoice_id, created_at DESC);
CREATE UNIQUE INDEX uq_invoice_payments_ledger_transfer ON billing.invoice_payments USING btree (merchant_id, ledger_transfer_id) WHERE (ledger_transfer_id IS NOT NULL);
CREATE UNIQUE INDEX uq_invoice_payments_settled_rail_payment ON billing.invoice_payments USING btree (merchant_id, psp_id, rail_payment_id) WHERE ((status = 'settled'::text) AND (psp_id IS NOT NULL) AND (rail_payment_id IS NOT NULL));
CREATE UNIQUE INDEX ux_invoice_payments_attempt_key ON billing.invoice_payments USING btree (merchant_id, invoice_id, idempotency_key) WHERE (idempotency_key IS NOT NULL);

ALTER TABLE ONLY billing.invoice_payments
    ADD CONSTRAINT invoice_payments_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.invoice_payments
    ADD CONSTRAINT invoice_payments_invoice_fk FOREIGN KEY (merchant_id, customer_id, currency, invoice_id) REFERENCES billing.invoices(merchant_id, customer_id, currency, id) ON DELETE CASCADE;
ALTER TABLE ONLY billing.invoice_payments
    ADD CONSTRAINT invoice_payments_ledger_transfer_fk FOREIGN KEY (merchant_id, customer_id, currency, ledger_transfer_id) REFERENCES billing.ledger_transfers(merchant_id, customer_id, currency, id);
ALTER TABLE ONLY billing.invoice_payments
    ADD CONSTRAINT invoice_payments_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.invoice_payments
    ADD CONSTRAINT invoice_payments_payment_method_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_method_id) REFERENCES billing.payment_methods(merchant_id, customer_id, id) ON DELETE SET NULL (payment_method_id);
ALTER TABLE ONLY billing.invoice_payments
    ADD CONSTRAINT invoice_payments_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id);

-- ---------------------------------------------------------------------------
-- Notifications and host events
-- ---------------------------------------------------------------------------

CREATE TABLE billing.notifications (
    id uuid DEFAULT uuidv7() NOT NULL,
    event_type text NOT NULL,
    data jsonb NOT NULL,
    recipient_kind text DEFAULT 'customer' NOT NULL,
    read_at timestamp with time zone,
    severity text DEFAULT '' NOT NULL,
    title text DEFAULT '' NOT NULL,
    body text DEFAULT '' NOT NULL,
    link text DEFAULT '' NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid,
    emailed_at timestamp with time zone,
    CONSTRAINT notifications_recipient CHECK (
        (recipient_kind = 'customer' AND customer_id IS NOT NULL AND severity = '' AND title = '' AND body = '' AND link = '')
        OR (recipient_kind = 'merchant' AND customer_id IS NULL AND event_type = 'operator.alert' AND emailed_at IS NULL AND title <> '')
    )
);
COMMENT ON TABLE billing.notifications IS 'Recipient-scoped customer and merchant notifications. read_at records inbox state; financial acknowledgments belong to host_outbox.';
COMMENT ON COLUMN billing.notifications.emailed_at IS '#789: when the notification email was sent; NULL = undelivered (the notification_email_sweep retries).';

ALTER TABLE ONLY billing.notifications
    ADD CONSTRAINT notifications_pkey PRIMARY KEY (id);

CREATE INDEX idx_notifications_created_at ON billing.notifications USING btree (created_at);
CREATE INDEX idx_notifications_customer ON billing.notifications USING btree (customer_id) WHERE (customer_id IS NOT NULL);
CREATE INDEX idx_notifications_event_type ON billing.notifications USING btree (event_type);
CREATE INDEX idx_notifications_merchant_id ON billing.notifications USING btree (merchant_id);
CREATE INDEX notifications_inbox_idx ON billing.notifications USING btree (merchant_id, recipient_kind, customer_id, read_at, created_at DESC);
CREATE INDEX idx_notifications_undelivered ON billing.notifications USING btree (merchant_id, created_at, id) WHERE (recipient_kind = 'customer' AND emailed_at IS NULL);
CREATE INDEX ix_notifications_retention ON billing.notifications USING btree (merchant_id, created_at);

ALTER TABLE ONLY billing.notifications
    ADD CONSTRAINT notifications_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id);
ALTER TABLE ONLY billing.notifications
    ADD CONSTRAINT notifications_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TABLE billing.host_outbox (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    event_type text NOT NULL,
    subject_type text NOT NULL,
    payment_id uuid,
    amount bigint,
    subject_id uuid NOT NULL,
    currency text NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    data jsonb DEFAULT '{}'::jsonb NOT NULL,
    delivered_at timestamp with time zone,
    dedupe_key text NOT NULL,
    CONSTRAINT host_outbox_currency_shape CHECK ((currency ~ '^[A-Z0-9]{3,12}$'::text)),
    CONSTRAINT host_outbox_payload CHECK (
        (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL
         AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
        OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared')
            AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    )
);
COMMENT ON TABLE billing.host_outbox IS 'Typed durable host events: successful rail payment settlements and delinquency lifecycle transitions. Acknowledge after idempotent processing; acknowledgments are separate from notification read state.';
COMMENT ON COLUMN billing.host_outbox.currency IS 'The transition''s currency. NOT NULL (CUR-1): every lifecycle event is per-(merchant, payer, currency) and the currency is part of its dedupe key, so an event without one is not a well-formed event.';
COMMENT ON COLUMN billing.host_outbox.dedupe_key IS 'Deterministic per transition (delinquency:<customer>:<currency>:<transition_seq>) so a re-run collapses instead of instructing a second shutoff.';

ALTER TABLE ONLY billing.host_outbox
    ADD CONSTRAINT host_outbox_pkey PRIMARY KEY (id);

CREATE INDEX ix_host_outbox_delivered ON billing.host_outbox USING btree (merchant_id, delivered_at) WHERE (delivered_at IS NOT NULL);
CREATE INDEX ix_host_outbox_pending ON billing.host_outbox USING btree (merchant_id, event_type, id) WHERE (delivered_at IS NULL);
CREATE UNIQUE INDEX uq_host_outbox_dedupe ON billing.host_outbox USING btree (merchant_id, dedupe_key);
CREATE UNIQUE INDEX uq_host_outbox_payment ON billing.host_outbox (merchant_id, payment_id) WHERE payment_id IS NOT NULL;

ALTER TABLE ONLY billing.host_outbox
    ADD CONSTRAINT host_outbox_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE;
ALTER TABLE ONLY billing.host_outbox
    ADD CONSTRAINT host_outbox_payment_fk FOREIGN KEY (merchant_id, payment_id) REFERENCES billing.payments(merchant_id, id) ON DELETE CASCADE;

-- ---------------------------------------------------------------------------
-- Reconciliation
-- ---------------------------------------------------------------------------

CREATE TABLE billing.reconciliation_state (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    source_domain text NOT NULL,
    fully_reconciled boolean DEFAULT false NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_reconciliation_state_domain CHECK ((source_domain = ANY (ARRAY['subscriptions'::text, 'payments'::text, 'grants'::text])))
);
COMMENT ON TABLE billing.reconciliation_state IS '#511 per-(merchant, source_domain) reconciliation watermark. fully_reconciled gates the confirmed-absence rule: a destructive EXCESS repair is HELD until its source domain (subscriptions|payments|grants) is proven fully reconciled.';

ALTER TABLE ONLY billing.reconciliation_state
    ADD CONSTRAINT reconciliation_state_pkey PRIMARY KEY (id);

CREATE UNIQUE INDEX uq_reconciliation_state_identity ON billing.reconciliation_state USING btree (merchant_id, source_domain);

ALTER TABLE ONLY billing.reconciliation_state
    ADD CONSTRAINT reconciliation_state_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

CREATE TABLE billing.reconciliation_findings (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    finding_type text NOT NULL,
    rail text DEFAULT '' NOT NULL,
    psp_id uuid,
    openrails_resource_type text DEFAULT '' NOT NULL,
    openrails_resource_id text,
    external_resource_id text,
    field text,
    openrails_value text,
    external_value text,
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
    seen_run_class text GENERATED ALWAYS AS (CASE WHEN first_seen_run IS NOT NULL OR last_seen_run IS NOT NULL THEN 'observation' END) STORED,
    CONSTRAINT reconciliation_findings_catalog_shape CHECK (
        (finding_type IN ('catalog.orphan_in_stripe','catalog.missing_in_stripe','catalog.orphan_in_nmi','catalog.missing_in_nmi','catalog.missing_in_solana','catalog.field_drift')
         AND rail IN ('stripe','nmi','solana') AND psp_id IS NOT NULL AND openrails_resource_type IN ('product','price')
         AND (finding_type = 'catalog.field_drift' OR finding_type LIKE 'catalog.%_in_' || rail)
         AND first_seen_run IS NULL AND last_seen_run IS NULL
         AND subject_key = jsonb_build_array(psp_id::text,openrails_resource_type,coalesce(openrails_resource_id,''),coalesce(external_resource_id,''),coalesce(field,''))::text)
        OR (finding_type NOT LIKE 'catalog.%' AND rail='' AND psp_id IS NULL AND openrails_resource_type=''
         AND openrails_resource_id IS NULL AND external_resource_id IS NULL AND field IS NULL
         AND openrails_value IS NULL AND external_value IS NULL)
    ),
    CONSTRAINT chk_reconciliation_findings_resolution CHECK (((resolution IS NULL) OR (resolution = ANY (ARRAY['auto_vanished'::text, 'enforced'::text, 'admin_fixed'::text, 'ignored'::text])))),
    CONSTRAINT chk_reconciliation_findings_resolved_fields CHECK ((((status = ANY (ARRAY['auto_fixed'::text, 'fixed'::text, 'ignored'::text])) AND (resolved_at IS NOT NULL) AND (resolution IS NOT NULL)) OR ((status = ANY (ARRAY['reconcile_required'::text, 'requires_review'::text])) AND (resolved_at IS NULL) AND (resolution IS NULL)))),
    CONSTRAINT chk_reconciliation_findings_severity CHECK ((severity = ANY (ARRAY['critical'::text, 'high'::text, 'medium'::text, 'low'::text]))),
    CONSTRAINT chk_reconciliation_findings_status CHECK ((status = ANY (ARRAY['auto_fixed'::text, 'reconcile_required'::text, 'requires_review'::text, 'fixed'::text, 'ignored'::text]))),
    CONSTRAINT chk_reconciliation_findings_type CHECK ((finding_type ~ '^(pull|derive|life|consistency|notify|catalog)\.[a-z0-9_]+(\.[a-z0-9_]+)?$'::text))
);
COMMENT ON TABLE billing.reconciliation_findings IS 'Durable reconciliation findings ledger. Stable identity per (merchant, finding_type, subject_key); provider/account context lives in evidence for pull.* findings. Statuses: reconcile_required, requires_review, auto_fixed, fixed, ignored (#573).';
COMMENT ON COLUMN billing.reconciliation_findings.subject_key IS 'Stable identity of the drifted subject within (provider, finding_type): rail subscription id, transaction id, local subscription/payment-method uuid, or tenant_subject uuid depending on the check.';
COMMENT ON COLUMN billing.reconciliation_findings.psp_id IS 'Catalog findings only: the immutable PSP account whose catalog was compared. Part of the identity; absence can be proven only by a complete read of this account.';
COMMENT ON COLUMN billing.reconciliation_findings.first_seen_run IS 'Reconciliation run that first observed this finding; NULL when raised outside a run (e.g. the intents volume breaker).';
COMMENT ON COLUMN billing.reconciliation_findings.operator_notes IS 'Operator-entered notes attached when a finding is fixed or ignored manually.';
COMMENT ON COLUMN billing.reconciliation_findings.evidence IS 'Machine-readable finding evidence. Optional nested keys: provider, local, remote, intent, resolution.';
COMMENT ON COLUMN billing.reconciliation_findings.resolved_by IS 'Authenticated admin identity stamped on manual resolution (approve/ignore via the findings queue, #692); NULL for automatic resolutions.';
COMMENT ON COLUMN billing.reconciliation_findings.notified_at IS '#787: last time this OPEN finding pushed an operator notification; NULL = not yet notified this open episode. Cleared to NULL on every resolution so a reopened finding notifies again.';
COMMENT ON COLUMN billing.reconciliation_findings.notified_severity IS '#787: severity at last notification; a further increase while still open re-fires, re-observation at the same/lower severity does not.';

ALTER TABLE ONLY billing.reconciliation_findings
    ADD CONSTRAINT reconciliation_findings_pkey PRIMARY KEY (id);

CREATE INDEX idx_reconciliation_findings_actionable ON billing.reconciliation_findings USING btree (finding_type) WHERE (status = ANY (ARRAY['reconcile_required'::text, 'requires_review'::text]));
CREATE INDEX idx_reconciliation_findings_low_severity_pending_digest ON billing.reconciliation_findings USING btree (merchant_id) WHERE ((status = 'requires_review'::text) AND (severity = 'low'::text) AND (notified_at IS NULL));
CREATE INDEX idx_reconciliation_findings_merchant_id ON billing.reconciliation_findings USING btree (merchant_id);
CREATE INDEX idx_reconciliation_findings_requires_review ON billing.reconciliation_findings USING btree (last_seen_at DESC) WHERE (status = 'requires_review'::text);
CREATE UNIQUE INDEX uq_reconciliation_findings_identity ON billing.reconciliation_findings USING btree (merchant_id, finding_type, subject_key);
CREATE INDEX idx_reconciliation_findings_open_catalog ON billing.reconciliation_findings USING btree (merchant_id, psp_id, openrails_resource_type, openrails_resource_id, rail) WHERE ((resolved_at IS NULL) AND (finding_type ~~ 'catalog.%'::text));

ALTER TABLE ONLY billing.reconciliation_findings
    ADD CONSTRAINT reconciliation_findings_first_seen_run_fk FOREIGN KEY (merchant_id, first_seen_run, seen_run_class) REFERENCES billing.maintenance_runs(merchant_id, id, run_class) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.reconciliation_findings
    ADD CONSTRAINT reconciliation_findings_last_seen_run_fk FOREIGN KEY (merchant_id, last_seen_run, seen_run_class) REFERENCES billing.maintenance_runs(merchant_id, id, run_class) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.reconciliation_findings
    ADD CONSTRAINT reconciliation_findings_psp_fk FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps(merchant_id, id, rail) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.reconciliation_findings
    ADD CONSTRAINT reconciliation_findings_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;

-- Read-only projection of the standing findings owner; no second lifecycle.
CREATE VIEW billing.catalog_drift_events WITH (security_invoker=true) AS
SELECT id,psp_id,rail,substr(finding_type,9)::text AS kind,openrails_resource_type,openrails_resource_id,
       external_resource_id,field,openrails_value,external_value,
       created_at AS detected_at,resolved_at,merchant_id
FROM billing.reconciliation_findings WHERE finding_type LIKE 'catalog.%';

CREATE VIEW billing.freeloader_episodes WITH (security_invoker='true') AS
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
                   FROM billing.payments r
                  WHERE ((r.merchant_id = e.merchant_id) AND (r.refunded_payment_id = p.id) AND (r.deleted_at IS NULL))), p.purchased_at) AS refund_effective_at,
            ( SELECT max(COALESCE(g.ends_at, 'infinity'::timestamp with time zone)) AS max
                   FROM billing.grants g
                  WHERE ((g.merchant_id = e.merchant_id) AND (g.customer_id = e.customer_id) AND (g.event = 'grant'::text) AND (g.kind = 'entitlement'::text) AND (g.starts_at <= now()) AND ((g.id = e.grant_id) OR ((g.source_id = (e.source_id)::text) AND (((e.source_type = 'subscription'::text) AND (g.source_type = 'subscription'::text)) OR ((e.source_type = 'one_off'::text) AND (g.source_type = 'purchase'::text))))) AND (NOT (EXISTS ( SELECT 1
                           FROM billing.grants t
                          WHERE ((t.merchant_id = g.merchant_id) AND (t.supersedes_id = g.id) AND (t.event = ANY (ARRAY['revoke'::text, 'expire'::text, 'supersede'::text])))))))) AS grant_covered_until
           FROM ((billing.entitlements e
             LEFT JOIN billing.subscriptions s ON (((e.source_type = 'subscription'::text) AND (s.id = e.source_id) AND (s.merchant_id = e.merchant_id) AND (s.deleted_at IS NULL))))
             LEFT JOIN billing.payments p ON (((e.source_type = 'one_off'::text) AND (p.id = e.source_id) AND (p.merchant_id = e.merchant_id) AND (p.deleted_at IS NULL))))
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
                    WHEN (w.payment_status = 'completed'::billing.payment_status) THEN 'infinity'::timestamp with time zone
                    WHEN (w.payment_status = 'refunded'::billing.payment_status) THEN w.refund_effective_at
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
            WHEN ((sub_status = 'past_due'::billing.subscription_status) AND (next_retry_at IS NOT NULL)) THEN 'sanctioned_dunning'::text
            WHEN (sub_status = 'unverified'::billing.subscription_status) THEN 'awaiting_verification'::text
            ELSE 'unsanctioned'::text
        END AS cause,
    unpaid_from AS started_at,
    unpaid_until AS ended_at,
    (window_end > now()) AS open,
    ((EXTRACT(epoch FROM (unpaid_until - unpaid_from)) / 86400.0))::double precision AS days
   FROM spans
  WHERE (unpaid_from < unpaid_until);
COMMENT ON VIEW billing.freeloader_episodes IS '#690 episode analytics: spans of entitlement access NOT covered by payment (subscription paid-through snapshot, completed one_off payment, or a live matching grant). Open episodes (window still granting) end at now(). Causes label sanctioned unpaid access (sanctioned_dunning, awaiting_verification) vs failure (unsanctioned). Approximations: paid-through is the current-period snapshot (renewals overwrite it, healed historical lapses are invisible); coverage is contiguous-from-the-left (uncovered TAIL only); cause reads the sub''s CURRENT state; refund time falls back to the purchase time when no refund row links.';

CREATE VIEW billing.orphaned_episodes WITH (security_invoker='true') AS
 WITH cov AS (
         SELECT s.merchant_id,
            s.customer_id,
            'subscription'::text AS source_type,
            s.id AS source_id,
            s.product_id,
            COALESCE(s.current_period_starts_at, s.started_at) AS cov_start,
            GREATEST(s.current_period_ends_at, s.ended_at) AS cov_end
           FROM (billing.subscriptions s
             JOIN billing.products pd ON (((pd.id = s.product_id) AND (pd.merchant_id = s.merchant_id))))
          WHERE ((s.deleted_at IS NULL) AND (s.status <> 'pending'::billing.subscription_status) AND (GREATEST(s.current_period_ends_at, s.ended_at) IS NOT NULL) AND (((pd.entitlements_spec IS NOT NULL) AND (pd.entitlements_spec <> '{}'::jsonb)) OR ((s.entitlements_spec_snapshot IS NOT NULL) AND (s.entitlements_spec_snapshot <> '{}'::jsonb))))
        UNION ALL
         SELECT p.merchant_id,
            p.customer_id,
            'one_off'::text AS text,
            p.id,
            pr.product_id,
            p.purchased_at,
            (p.purchased_at + make_interval(hours => pr.access_duration_hours))
           FROM ((billing.payments p
             JOIN billing.prices pr ON (((pr.id = p.price_id) AND (pr.merchant_id = p.merchant_id))))
             JOIN billing.products pd ON (((pd.id = pr.product_id) AND (pd.merchant_id = p.merchant_id))))
          WHERE ((p.deleted_at IS NULL) AND (p.status = 'completed'::billing.payment_status) AND (p.amount > 0) AND (p.subscription_id IS NULL) AND (pr.access_duration_hours IS NOT NULL) AND (pd.entitlements_spec IS NOT NULL) AND (pd.entitlements_spec <> '{}'::jsonb))
        ), spans AS (
         SELECT c.merchant_id,
            c.customer_id,
            c.source_type,
            c.source_id,
            c.product_id,
            c.cov_start,
            c.cov_end,
            GREATEST(c.cov_start, COALESCE(( SELECT max(LEAST(COALESCE(e.revoked_at, 'infinity'::timestamp with time zone), COALESCE(e.deleted_at, 'infinity'::timestamp with time zone), COALESCE(e.end_at, 'infinity'::timestamp with time zone))) AS max
                   FROM billing.entitlements e
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
COMMENT ON VIEW billing.orphaned_episodes IS '#690 episode analytics, the mirror of freeloader_episodes: spans where payment coverage existed (subscription paid-through snapshot, or a completed one_off payment with a finite access window for an entitlement-promising product) but no entitlement window covered the time. Open episodes (paid-through still in the future) end at now(). Same approximations: paid-through is the current-period snapshot; window coverage is contiguous-from-the-left (uncovered TAIL only — a wrongly-early revocation shows as the tail from revoked_at to paid-through).';

-- ---------------------------------------------------------------------------
-- Cross-merchant work queues
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.account_updater_open_batch_merchant_ids(p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT b.merchant_id
      FROM billing.account_updater_batches b
     WHERE b.status IN ('pending', 'submitted')
     GROUP BY b.merchant_id
     -- Oldest open batch first: at cap, the merchant who has waited longest is
     -- served, never an arbitrary head (the or#837 fairness fix).
     ORDER BY MIN(b.created_at)
     LIMIT p_limit;
END;
$$;
COMMENT ON FUNCTION billing.account_updater_open_batch_merchant_ids(p_limit integer) IS 'or#795: merchants with an account-updater batch the custodian still owes results for — the ingest fan-out. Ids only; the poll, the download and the fold all run per-merchant under RunInMerchantScope.';
REVOKE ALL ON FUNCTION billing.account_updater_open_batch_merchant_ids(p_limit integer) FROM PUBLIC;

CREATE FUNCTION billing.account_updater_work_merchant_ids(p_custodian text, p_environment text, p_now timestamp with time zone, p_default_lookahead_days integer, p_after uuid, p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $_$
BEGIN
    RETURN QUERY
    SELECT c.merchant_id
      FROM billing.custodians c
      -- The window is the CUSTODIAN's own declared lookahead (or the caller's
      -- default), so this queue and the per-merchant pass select the same
      -- instruments from the same number. A value that is not a plain integer
      -- cannot reach here through the settings validator; if one ever does, the
      -- default stands rather than the whole fan-out raising.
      CROSS JOIN LATERAL (
            SELECT make_interval(days => COALESCE(
                       CASE WHEN c.settings ->> 'account_updater_lookahead_days' ~ '^[0-9]+$'
                            THEN (c.settings ->> 'account_updater_lookahead_days')::int END,
                       p_default_lookahead_days)) AS lookahead
           ) w
     WHERE c.kind = lower(p_custodian)
       AND c.environment = p_environment
       AND NOT c.archived
       -- The contracted add-on. Unarmed custodians are not work: calling the
       -- account-updater API without it is a 403, not a refresh.
       AND COALESCE(c.settings ->> 'account_updater', 'false') IN ('true', 't', '1')
       AND (p_after IS NULL OR c.merchant_id > p_after)
       -- One open batch at a time per custodian: a merchant already waiting on
       -- the network has no NEW work, only results to ingest.
       AND NOT EXISTS (
             SELECT 1 FROM billing.account_updater_batches b
              WHERE b.custodian_id = c.id AND b.merchant_id = c.merchant_id
                AND b.status IN ('pending', 'submitted'))
       AND EXISTS (
             SELECT 1
               FROM billing.payment_methods pm
              WHERE pm.merchant_id = c.merchant_id
                AND pm.custodian = c.kind AND pm.custodian_id = c.id
                AND pm.rail_method_ref <> ''
                AND (pm.account_updater_checked_at IS NULL
                     OR pm.account_updater_checked_at < p_now - w.lookahead)
                AND EXISTS (
                      SELECT 1
                        FROM billing.subscriptions s
                       WHERE s.payment_method_id = pm.id AND s.merchant_id = pm.merchant_id
                         AND s.deleted_at IS NULL
                         AND s.status IN ('active', 'past_due')
                         AND s.current_period_ends_at IS NOT NULL
                         AND s.current_period_ends_at <= p_now + w.lookahead))
     ORDER BY c.merchant_id
     LIMIT p_limit;
END;
$_$;
COMMENT ON FUNCTION billing.account_updater_work_merchant_ids(p_custodian text, p_environment text, p_now timestamp with time zone, p_default_lookahead_days integer, p_after uuid, p_limit integer) IS 'or#795: merchants whose ARMED custodian holds an instrument that backs a subscription renewing inside the custodian''s lookahead window and has not been refreshed since the last cycle — the fan-out list for AccountUpdaterWorker. Starts at the custodian registry, so a merchant that never signed up for the add-on costs one index probe and never reaches its payment methods. Ids only, after a cursor, capped.';
REVOKE ALL ON FUNCTION billing.account_updater_work_merchant_ids(p_custodian text, p_environment text, p_now timestamp with time zone, p_default_lookahead_days integer, p_after uuid, p_limit integer) FROM PUBLIC;

CREATE FUNCTION billing.count_destructive_intents_by_actor_since(p_actor text, p_intent_types text[], p_since timestamp with time zone) RETURNS bigint
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n
      FROM billing.rail_intents
     WHERE actor = p_actor
       AND origin IN ('user', 'admin')
       AND intent_type = ANY (p_intent_types)
       AND created_at >= p_since;
    RETURN n;
END;
$$;
COMMENT ON FUNCTION billing.count_destructive_intents_by_actor_since(p_actor text, p_intent_types text[], p_since timestamp with time zone) IS 'Per-actor, cross-merchant count of destructive intents in the rolling window — the #732 ceiling''s more specific leg. One compromised credential operating across merchants is exactly the shape this must see.';
REVOKE ALL ON FUNCTION billing.count_destructive_intents_by_actor_since(p_actor text, p_intent_types text[], p_since timestamp with time zone) FROM PUBLIC;

CREATE FUNCTION billing.count_destructive_intents_for_merchant_since(p_merchant uuid, p_origins text[], p_intent_types text[], p_since timestamp with time zone) RETURNS bigint
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n
      FROM billing.rail_intents
     WHERE merchant_id = p_merchant
       AND origin = ANY (p_origins)
       AND intent_type = ANY (p_intent_types)
       AND created_at >= p_since;
    RETURN n;
END;
$$;
COMMENT ON FUNCTION billing.count_destructive_intents_for_merchant_since(p_merchant uuid, p_origins text[], p_intent_types text[], p_since timestamp with time zone) IS 'ONE merchant''s destructive intents in a rolling window, for a caller-supplied origin set — both legs of the #732 ceiling: the anti-theft wall (user/admin, or#887) and the automation wall (system, or#842). Definer, not a base-pool read: the gate holds the root pool and carries no app.merchant_id, where a GUC-less count would return 0 and fail open.';
REVOKE ALL ON FUNCTION billing.count_destructive_intents_for_merchant_since(p_merchant uuid, p_origins text[], p_intent_types text[], p_since timestamp with time zone) FROM PUBLIC;

CREATE FUNCTION billing.customer_merchant_ids_for_subject(p_subject uuid) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT DISTINCT c.merchant_id
      FROM billing.customers c
     WHERE c.id = p_subject;
END;
$$;
COMMENT ON FUNCTION billing.customer_merchant_ids_for_subject(p_subject uuid) IS 'Merchants where an AuthKit subject holds a customer record, across every merchant scope. For the hosted portal''s "your merchants" list, which runs before any merchant is chosen.';
REVOKE ALL ON FUNCTION billing.customer_merchant_ids_for_subject(p_subject uuid) FROM PUBLIC;

CREATE FUNCTION billing.delinquency_work_merchant_ids(p_now timestamp with time zone, p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT q.mid
      FROM (
            SELECT DISTINCT i.merchant_id AS mid
              FROM billing.invoices i
             WHERE i.status IN ('open', 'past_due')
               AND i.amount_due > 0
               AND i.due_at IS NOT NULL
               AND i.due_at < p_now
            UNION
            SELECT DISTINCT d.merchant_id AS mid
              FROM billing.customer_delinquency d
             WHERE d.state <> 'current'
           ) q
     LIMIT p_limit;
END;
$$;
COMMENT ON FUNCTION billing.delinquency_work_merchant_ids(p_now timestamp with time zone, p_limit integer) IS 'or#878: merchants with arrears delinquency work — an overdue open receivable (the enter leg) or a payer already parked non-current (the exit leg). The fan-out list for DelinquencyWorker. Ids only; states, transitions and signals are computed per-merchant under RunInMerchantScope.';
REVOKE ALL ON FUNCTION billing.delinquency_work_merchant_ids(p_now timestamp with time zone, p_limit integer) FROM PUBLIC;

CREATE FUNCTION billing.due_dunning_merchant_ids(p_rails text[], p_now timestamp with time zone, p_limit integer, p_include_engine boolean) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT s.merchant_id
      FROM billing.subscriptions s
     WHERE s.rail = ANY(p_rails)
       AND ((s.collection_policy <> 'engine' AND s.rail='nmi' AND s.status='past_due' AND s.next_retry_at IS NOT NULL AND s.next_retry_at <= p_now)
            OR (s.status='awaiting_method' AND s.grace_ends_at <= p_now
                AND ((p_include_engine AND s.collection_policy='engine') OR (s.collection_policy='nmi_schedule' AND s.rail='nmi')))
            OR (p_include_engine AND s.collection_policy='engine' AND s.current_period_ends_at <= p_now
                AND (s.status='active' OR (s.status='past_due' AND s.next_retry_at <= p_now))
                AND NOT EXISTS (SELECT 1 FROM billing.rail_intents i WHERE i.merchant_id=s.merchant_id AND i.subscription_id=s.id AND i.intent_type='subscription_collection' AND i.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable'))))
       AND s.deleted_at IS NULL
     GROUP BY s.merchant_id
     ORDER BY MIN(CASE WHEN s.status='awaiting_method' THEN s.grace_ends_at WHEN s.collection_policy='engine' AND s.status='active' THEN s.current_period_ends_at ELSE s.next_retry_at END), s.merchant_id
     LIMIT p_limit;
END;
$$;
COMMENT ON FUNCTION billing.due_dunning_merchant_ids(p_rails text[], p_now timestamp with time zone, p_limit integer, p_include_engine boolean) IS 'Merchants with a due past_due subscription on the named rails — the fan-out list for DunningWorker. Ids only; the due rows, the charges and every lifecycle transition run per-merchant under RunInMerchantScope. Replaces a bare-context scan that returned an empty slice on every run, so scheduled dunning (retries, #839 staleness parking, #840 terminal handling) never fired at all (or#877 B5).';
REVOKE ALL ON FUNCTION billing.due_dunning_merchant_ids(p_rails text[], p_now timestamp with time zone, p_limit integer, p_include_engine boolean) FROM PUBLIC;

CREATE FUNCTION billing.lapsed_credit_lot_merchant_ids(p_as_of timestamp with time zone, p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT DISTINCT g.merchant_id
      FROM billing.grants g
     WHERE g.kind = 'credit' AND g.event = 'grant'
       AND g.ends_at IS NOT NULL AND g.ends_at <= p_as_of
       AND NOT EXISTS (
             SELECT 1 FROM billing.grants tt
              WHERE tt.supersedes_id = g.id AND tt.merchant_id = g.merchant_id AND tt.event IN ('revoke', 'supersede')
           )
     LIMIT p_limit;
END;
$$;
COMMENT ON FUNCTION billing.lapsed_credit_lot_merchant_ids(p_as_of timestamp with time zone, p_limit integer) IS 'Merchants holding at least one past-expiry, non-superseded credit lot — the fan-out list for CreditExpiryWorker. Ids only; the per-customer work list and the ledger claw-back run per-merchant. Replaces a base-pool enumeration that returned nothing, so no credit lot has ever been expired (or#868 B1).';
REVOKE ALL ON FUNCTION billing.lapsed_credit_lot_merchant_ids(p_as_of timestamp with time zone, p_limit integer) FROM PUBLIC;

-- The merchants holding a subscription whose period ended before its owner's
-- deadline with neither an attempt nor a recorded miss for that cycle.
CREATE FUNCTION billing.overdue_rebill_merchant_ids(p_engine_cutoff timestamp with time zone, p_nmi_cutoff timestamp with time zone, p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT s.merchant_id
      FROM billing.subscriptions s
     WHERE s.status IN ('active', 'unverified', 'awaiting_method') AND s.deleted_at IS NULL
       AND ((s.collection_policy = 'engine' AND s.current_period_ends_at <= p_engine_cutoff)
            OR (s.collection_policy = 'nmi_schedule' AND s.current_period_ends_at <= p_nmi_cutoff))
       AND NOT EXISTS (
           SELECT 1 FROM billing.rebill_cycles c
            WHERE c.merchant_id = s.merchant_id AND c.subscription_id = s.id AND c.due_at = s.current_period_ends_at
              AND c.missed_at IS NOT NULL)
       AND NOT EXISTS (
           SELECT 1 FROM billing.rebill_cycles c
             JOIN billing.payment_attempts a ON a.merchant_id = c.merchant_id AND a.cycle_id = c.id
            WHERE c.merchant_id = s.merchant_id AND c.subscription_id = s.id AND c.due_at = s.current_period_ends_at)
     GROUP BY s.merchant_id
     ORDER BY MIN(s.current_period_ends_at), s.merchant_id
     LIMIT p_limit;
END;
$$;
REVOKE ALL ON FUNCTION billing.overdue_rebill_merchant_ids(p_engine_cutoff timestamp with time zone, p_nmi_cutoff timestamp with time zone, p_limit integer) FROM PUBLIC;

CREATE FUNCTION billing.pending_merchant_secret_cleanups(p_after uuid, p_limit integer) RETURNS TABLE(merchant_id uuid, run_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN

 RETURN QUERY SELECT r.merchant_id,r.id FROM billing.maintenance_runs r
 JOIN billing.merchants m ON m.id=r.merchant_id
 WHERE r.kind='merchant_purge' AND r.status IN ('running','failed')
   AND r.affected->>'database_purged'='true' AND r.coverage ? 'secret_cleanup'
   AND m.deleted_at IS NOT NULL AND (p_after IS NULL OR r.id>p_after)
 ORDER BY r.id LIMIT p_limit;
END;
$$;
REVOKE ALL ON FUNCTION billing.pending_merchant_secret_cleanups(p_after uuid, p_limit integer) FROM PUBLIC;

CREATE FUNCTION billing.psp_rail_merchant_ids(p_rails text[], p_limit integer, p_after uuid) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT DISTINCT p.merchant_id
      FROM billing.psps p
      JOIN billing.merchants m ON m.id = p.merchant_id
     WHERE p.rail = ANY(p_rails)
       AND p.archived = false
       AND m.deleted_at IS NULL
       AND (p_after IS NULL OR p.merchant_id > p_after)
     ORDER BY p.merchant_id
     LIMIT p_limit;
END;
$$;
COMMENT ON FUNCTION billing.psp_rail_merchant_ids(p_rails text[], p_limit integer, p_after uuid) IS 'Ordered page of merchants after p_after, armed on at least one of the named rails (live PSP, undeleted merchant) — the fan-out list for StripeWebhookReconcileWorker and the alert-only catalog pull. Ids only; the PSP rows are read per-merchant under RunInMerchantScope. Replaces a merchants JOIN psps on the base pool, where the psps side is RLS-FORCED and always matched nothing — so the managed Stripe endpoint was never registered or version-bumped (or#877 B6).';
REVOKE ALL ON FUNCTION billing.psp_rail_merchant_ids(p_rails text[], p_limit integer, p_after uuid) FROM PUBLIC;

CREATE FUNCTION billing.redrivable_plan_change_merchant_ids(p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT DISTINCT r.merchant_id
      FROM billing.subscription_reprices r
     WHERE r.kind = 'plan_change'
       AND r.status = 'blocked'
       AND r.blocked_reason LIKE 'rail_push_failed:%'
     LIMIT p_limit;
END;
$$;
COMMENT ON FUNCTION billing.redrivable_plan_change_merchant_ids(p_limit integer) IS '#816: merchants holding a rail-push-blocked plan_change reprice. Ids only — unlike the armed scans the re-driver needs whole rows, and a definer must not vend those, so the rows are read per-merchant under RunInMerchantConn (or#861).';
REVOKE ALL ON FUNCTION billing.redrivable_plan_change_merchant_ids(p_limit integer) FROM PUBLIC;

CREATE FUNCTION billing.retention_work_merchant_ids(p_now timestamp with time zone, p_notification_cutoff timestamp with time zone, p_notification_seen_cutoff timestamp with time zone, p_webhook_cutoff timestamp with time zone, p_settlement_cutoff timestamp with time zone, p_lifecycle_cutoff timestamp with time zone, p_attempt_cutoff timestamp with time zone, p_after uuid, p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT q.mid
      FROM (
            (SELECT DISTINCT cs.merchant_id AS mid
               FROM billing.checkout_sessions cs
              WHERE (p_after IS NULL OR cs.merchant_id > p_after)
                AND cs.expires_at IS NOT NULL AND cs.expires_at < p_now
                AND cs.deleted_at IS NULL
                AND cs.status IN ('created', 'requires_action')
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT nq.merchant_id AS mid
               FROM billing.notifications nq
              WHERE (p_after IS NULL OR nq.merchant_id > p_after)
                AND (nq.created_at < p_notification_cutoff
                     OR (nq.read_at IS NOT NULL AND nq.created_at < p_notification_seen_cutoff))
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT we.merchant_id AS mid
               FROM billing.webhook_events we
              WHERE (p_after IS NULL OR we.merchant_id > p_after)
                AND we.completed_at < p_webhook_cutoff
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT pse.merchant_id AS mid
               FROM billing.host_outbox pse
              WHERE (p_after IS NULL OR pse.merchant_id > p_after)
                AND pse.event_type = 'payment.settled' AND pse.delivered_at IS NOT NULL
                AND pse.delivered_at < p_settlement_cutoff
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT hle.merchant_id AS mid
               FROM billing.host_outbox hle
              WHERE (p_after IS NULL OR hle.merchant_id > p_after)
                AND hle.event_type <> 'payment.settled' AND hle.delivered_at IS NOT NULL
                AND hle.delivered_at < p_lifecycle_cutoff
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT pa.merchant_id AS mid
               FROM billing.payment_attempts pa
              WHERE (p_after IS NULL OR pa.merchant_id > p_after)
                AND pa.attempted_at < p_attempt_cutoff
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT rc.merchant_id AS mid
               FROM billing.rebill_cycles rc
              WHERE (p_after IS NULL OR rc.merchant_id > p_after)
                AND rc.due_at < p_attempt_cutoff
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT nh.merchant_id AS mid
               FROM billing.nmi_history_months nh
              WHERE (p_after IS NULL OR nh.merchant_id > p_after)
                AND nh.month < p_attempt_cutoff
              ORDER BY 1 LIMIT p_limit)
           ) q
     ORDER BY q.mid
     LIMIT p_limit;
END;
$$;
COMMENT ON FUNCTION billing.retention_work_merchant_ids(p_now timestamp with time zone, p_notification_cutoff timestamp with time zone, p_notification_seen_cutoff timestamp with time zone, p_webhook_cutoff timestamp with time zone, p_settlement_cutoff timestamp with time zone, p_lifecycle_cutoff timestamp with time zone, p_attempt_cutoff timestamp with time zone, p_after uuid, p_limit integer) IS 'or#837: merchants with retention work: an expirable checkout session past its TTL, a notification/webhook-dedup row past its window, an ACKED settlement/host-lifecycle event past its prune age, or a payment attempt/rebill cycle/NMI history month past its retention (#1118, #1120). The fan-out list for CleanupExpiredDataWorker; ids only, after a cursor, capped; the deletes run per-merchant in bounded batches.';
REVOKE ALL ON FUNCTION billing.retention_work_merchant_ids(p_now timestamp with time zone, p_notification_cutoff timestamp with time zone, p_notification_seen_cutoff timestamp with time zone, p_webhook_cutoff timestamp with time zone, p_settlement_cutoff timestamp with time zone, p_lifecycle_cutoff timestamp with time zone, p_attempt_cutoff timestamp with time zone, p_after uuid, p_limit integer) FROM PUBLIC;

-- The merchants holding NMI attempts in [p_since, p_before) not yet enriched.
CREATE FUNCTION billing.unenriched_attempt_merchant_ids(p_since timestamp with time zone, p_before timestamp with time zone, p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT a.merchant_id
      FROM billing.payment_attempts a
     WHERE a.enriched_at IS NULL AND a.rail = 'nmi' AND a.transaction_id IS NOT NULL
       AND a.attempted_at >= p_since AND a.attempted_at < p_before
     GROUP BY a.merchant_id
     ORDER BY MIN(a.attempted_at), a.merchant_id
     LIMIT p_limit;
END;
$$;
REVOKE ALL ON FUNCTION billing.unenriched_attempt_merchant_ids(p_since timestamp with time zone, p_before timestamp with time zone, p_limit integer) FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- Fleet analytics
-- ---------------------------------------------------------------------------

CREATE FUNCTION billing.fleet_merchant_funnel(p_exclude uuid, p_since timestamp with time zone) RETURNS TABLE(total bigint, armed bigint, first_revenue bigint, active_revenue bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT count(*)::bigint,
           (count(*) FILTER (WHERE EXISTS (
               SELECT 1 FROM billing.psps p
                WHERE p.merchant_id = m.id AND p.replaced_at IS NULL)))::bigint,
           (count(*) FILTER (WHERE EXISTS (
               SELECT 1 FROM billing.payments pay
                WHERE pay.merchant_id = m.id AND pay.status = 'completed'
                  AND pay.reversal_kind IS NULL)))::bigint,
           (count(*) FILTER (WHERE EXISTS (
               SELECT 1 FROM billing.payments pay
                WHERE pay.merchant_id = m.id AND pay.status = 'completed'
                  AND pay.reversal_kind IS NULL
                  AND pay.purchased_at >= p_since)))::bigint
      FROM billing.merchants m
     WHERE m.deleted_at IS NULL AND m.status = 'active'
       AND (p_exclude IS NULL OR m.id <> p_exclude);
END;
$$;
COMMENT ON FUNCTION billing.fleet_merchant_funnel(p_exclude uuid, p_since timestamp with time zone) IS 'Fleet funnel counters (provisioned / armed / ever-earned / earning-now). Four scalars, no rows (or#861).';
REVOKE ALL ON FUNCTION billing.fleet_merchant_funnel(p_exclude uuid, p_since timestamp with time zone) FROM PUBLIC;

CREATE FUNCTION billing.fleet_mrr_by_currency(p_exclude uuid) RETURNS TABLE(currency text, subscriptions bigint, monthly_amount bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT pr.currency::text, count(*)::bigint,
           COALESCE(sum(billing.monthly_normalized_amount(pr.amount, pr.access_duration_hours)), 0)::bigint
      FROM billing.subscriptions s
      JOIN billing.prices pr ON pr.id = s.price_id AND pr.merchant_id = s.merchant_id
     WHERE s.status = 'active' AND pr.auto_renew AND pr.access_duration_hours > 0
       AND (p_exclude IS NULL OR s.merchant_id <> p_exclude)
     GROUP BY pr.currency ORDER BY pr.currency;
END;
$$;
COMMENT ON FUNCTION billing.fleet_mrr_by_currency(p_exclude uuid) IS 'Fleet MRR per currency using monthly_normalized_amount, the dashboard mrr definition (or#861, #1069).';
REVOKE ALL ON FUNCTION billing.fleet_mrr_by_currency(p_exclude uuid) FROM PUBLIC;

CREATE FUNCTION billing.fleet_rail_health(p_exclude uuid, p_since timestamp with time zone) RETURNS TABLE(rail text, succeeded bigint, failed bigint, chargebacks bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    WITH charges AS (
        SELECT a.rail AS r,
               count(*) FILTER (WHERE a.category = 'approved') AS ok,
               count(*) FILTER (WHERE a.category <> 'approved') AS refused
          FROM billing.payment_attempts a
         WHERE a.attempted_at >= p_since AND a.kind <> 'verify'
           AND (p_exclude IS NULL OR a.merchant_id <> p_exclude)
         GROUP BY a.rail
    ), disputes AS (
        SELECT p.rail::text AS r, count(*) AS n
          FROM billing.payments p
         WHERE p.purchased_at >= p_since AND p.reversal_kind = 'chargeback' AND p.status = 'completed'
           AND (p_exclude IS NULL OR p.merchant_id <> p_exclude)
         GROUP BY p.rail
    )
    SELECT COALESCE(c.r, d.r), COALESCE(c.ok, 0)::bigint, COALESCE(c.refused, 0)::bigint, COALESCE(d.n, 0)::bigint
      FROM charges c FULL JOIN disputes d ON d.r = c.r
     ORDER BY 1;
END;
$$;
COMMENT ON FUNCTION billing.fleet_rail_health(p_exclude uuid, p_since timestamp with time zone) IS 'Per-rail fleet approved/declined charge attempts and chargebacks in the window (or#861, #1111).';
REVOKE ALL ON FUNCTION billing.fleet_rail_health(p_exclude uuid, p_since timestamp with time zone) FROM PUBLIC;

CREATE FUNCTION billing.fleet_revenue_by_currency(p_exclude uuid, p_since timestamp with time zone) RETURNS TABLE(currency text, payments bigint, settled_amount bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT p.currency::text, count(*)::bigint, COALESCE(sum(p.amount), 0)::bigint
      FROM billing.payments p
     WHERE p.status = 'completed' AND p.reversal_kind IS NULL AND p.purchased_at >= p_since
       AND (p_exclude IS NULL OR p.merchant_id <> p_exclude)
     GROUP BY p.currency ORDER BY p.currency;
END;
$$;
COMMENT ON FUNCTION billing.fleet_revenue_by_currency(p_exclude uuid, p_since timestamp with time zone) IS 'Settled fleet sale volume per currency in the window. Sale rows only — reversal mirror rows share status=completed and must never count (or#861).';
REVOKE ALL ON FUNCTION billing.fleet_revenue_by_currency(p_exclude uuid, p_since timestamp with time zone) FROM PUBLIC;

CREATE FUNCTION billing.fleet_weekly_active_merchants(p_exclude uuid, p_since timestamp with time zone) RETURNS TABLE(week_start timestamp with time zone, merchants bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT date_trunc('week', p.purchased_at), (count(DISTINCT p.merchant_id))::bigint
      FROM billing.payments p
     WHERE p.status = 'completed' AND p.reversal_kind IS NULL
       AND p.purchased_at >= date_trunc('week', p_since)
       AND (p_exclude IS NULL OR p.merchant_id <> p_exclude)
     GROUP BY 1;
END;
$$;
COMMENT ON FUNCTION billing.fleet_weekly_active_merchants(p_exclude uuid, p_since timestamp with time zone) IS 'Weekly count of DISTINCT merchants with a settled sale — a count per ISO week, never the merchant list (or#861).';
REVOKE ALL ON FUNCTION billing.fleet_weekly_active_merchants(p_exclude uuid, p_since timestamp with time zone) FROM PUBLIC;

CREATE FUNCTION billing.fleet_weekly_cancelled_subscriptions(p_exclude uuid, p_since timestamp with time zone) RETURNS TABLE(week_start timestamp with time zone, cancellations bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT date_trunc('week', s.cancelled_at), count(*)::bigint
      FROM billing.subscriptions s
     WHERE s.cancelled_at IS NOT NULL
       AND s.cancelled_at >= date_trunc('week', p_since)
       AND (p_exclude IS NULL OR s.merchant_id <> p_exclude)
     GROUP BY 1;
END;
$$;
COMMENT ON FUNCTION billing.fleet_weekly_cancelled_subscriptions(p_exclude uuid, p_since timestamp with time zone) IS 'Weekly fleet subscription cancellations — the churn proxy on the fleet trend chart (or#861).';
REVOKE ALL ON FUNCTION billing.fleet_weekly_cancelled_subscriptions(p_exclude uuid, p_since timestamp with time zone) FROM PUBLIC;

CREATE FUNCTION billing.fleet_weekly_volume(p_exclude uuid, p_since timestamp with time zone) RETURNS TABLE(week_start timestamp with time zone, currency text, payments bigint, settled_amount bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'billing', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT date_trunc('week', p.purchased_at), p.currency::text, count(*)::bigint, COALESCE(sum(p.amount), 0)::bigint
      FROM billing.payments p
     WHERE p.status = 'completed' AND p.reversal_kind IS NULL
       AND p.purchased_at >= date_trunc('week', p_since)
       AND (p_exclude IS NULL OR p.merchant_id <> p_exclude)
     GROUP BY 1, 2 ORDER BY 1, 2;
END;
$$;
COMMENT ON FUNCTION billing.fleet_weekly_volume(p_exclude uuid, p_since timestamp with time zone) IS 'Weekly settled fleet sale volume per currency. Sale rows only (or#861).';
REVOKE ALL ON FUNCTION billing.fleet_weekly_volume(p_exclude uuid, p_since timestamp with time zone) FROM PUBLIC;
