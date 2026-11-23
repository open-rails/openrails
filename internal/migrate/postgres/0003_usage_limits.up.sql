-- Usage limits: catalog limit definitions, the capacity products give, a
-- customer's overrides, and each customer's limit windows.

CREATE TABLE billing.catalog_limits (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    key text NOT NULL CHECK (key ~ '^[a-z][a-z0-9_-]{0,63}$' AND key <> 'balance'),
    unit text NOT NULL CHECK (unit ~ '^[a-z][a-z0-9_]{0,63}$' OR unit ~ '^[A-Z0-9]{3,12}$'),
    reset_kind text NOT NULL CHECK (reset_kind IN ('fixed', 'billing_period', 'session', 'rolling', 'rate', 'lifetime')),
    window_seconds integer CHECK (window_seconds BETWEEN 1 AND 31622400),
    reset_anchor text CHECK (reset_anchor IN ('subscription', 'first_use')),
    overrun text NOT NULL CHECK (overrun IN ('require_estimate', 'admit_while_positive')),
    rate_cards jsonb NOT NULL CHECK (jsonb_typeof(rate_cards) = 'array'),
    revision bigint NOT NULL CHECK (revision >= 1),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, key),
    CONSTRAINT catalog_limits_window_check CHECK ((window_seconds IS NOT NULL) = (reset_kind IN ('fixed', 'session', 'rolling', 'rate'))),
    CONSTRAINT catalog_limits_anchor_check CHECK ((reset_anchor IS NOT NULL) = (reset_kind = 'fixed')),
    CONSTRAINT catalog_limits_currency_rate_cards_check CHECK (unit ~ '^[a-z]' OR rate_cards = '[]'::jsonb)
);
COMMENT ON TABLE billing.catalog_limits IS 'Usage limits the merchant declares: what a limit counts (its unit and rate cards), when its window resets, and how a request that crosses it is admitted. Capacity comes from products (catalog_product_limits) or a customer override. The built-in limit balance is never declared.';
COMMENT ON COLUMN billing.catalog_limits.unit IS 'A named unit (lowercase) or a currency code. A currency limit counts the rated amount and has no rate cards.';
COMMENT ON COLUMN billing.catalog_limits.window_seconds IS 'The window of a fixed, session, rolling or rate limit; null for billing_period and lifetime.';
COMMENT ON COLUMN billing.catalog_limits.reset_anchor IS 'What a fixed window tiles from: the customer''s subscription or its first use; null for every other reset.';
COMMENT ON COLUMN billing.catalog_limits.rate_cards IS 'How usage counts toward the limit: rate cards (meter, filter, price) priced in the limit''s unit, in declaration order.';
COMMENT ON COLUMN billing.catalog_limits.revision IS 'Advances on every change to the limit.';

CREATE TRIGGER catalog_authored_limit BEFORE INSERT OR DELETE OR UPDATE ON billing.catalog_limits
    FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();
CREATE TRIGGER catalog_limit_revision BEFORE INSERT OR UPDATE ON billing.catalog_limits
    FOR EACH ROW EXECUTE FUNCTION billing.assign_object_revision();

CREATE TABLE billing.catalog_product_limits (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    product_id uuid NOT NULL,
    limit_key text NOT NULL,
    capacity bigint NOT NULL CHECK (capacity >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, product_id, limit_key),
    CONSTRAINT catalog_product_limits_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT catalog_product_limits_limit_key_fkey FOREIGN KEY (merchant_id, limit_key) REFERENCES billing.catalog_limits (merchant_id, key) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.catalog_product_limits IS 'The capacity a product gives a limit in each window. A customer''s capacity is the sum over its live products, unless an override replaces it.';

CREATE INDEX catalog_product_limits_limit_key_idx ON billing.catalog_product_limits (merchant_id, limit_key);

CREATE TRIGGER catalog_authored_product_limit BEFORE INSERT OR DELETE OR UPDATE ON billing.catalog_product_limits
    FOR EACH ROW EXECUTE FUNCTION billing.catalog_authored_write();

CREATE TABLE billing.customer_limit_overrides (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    limit_key text NOT NULL,
    capacity bigint NOT NULL CHECK (capacity >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, limit_key),
    CONSTRAINT customer_limit_overrides_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT customer_limit_overrides_limit_key_fkey FOREIGN KEY (merchant_id, limit_key) REFERENCES billing.catalog_limits (merchant_id, key) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.customer_limit_overrides IS 'A customer''s own capacity for a limit, in place of the sum its products give; no row inherits that sum.';

CREATE INDEX customer_limit_overrides_limit_key_idx ON billing.customer_limit_overrides (merchant_id, limit_key);

CREATE TABLE billing.limit_windows (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    limit_key text NOT NULL CHECK (limit_key ~ '^[a-z][a-z0-9_-]{0,63}$' OR limit_key ~ '^spend:[0-9a-f]{64}$'),
    window_starts_at timestamptz NOT NULL,
    window_ends_at timestamptz,
    capacity bigint NOT NULL,
    settled bigint NOT NULL,
    adjusted bigint NOT NULL,
    reached_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, limit_key, window_starts_at),
    CONSTRAINT limit_windows_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT limit_windows_window_check CHECK (window_ends_at IS NULL OR window_ends_at > window_starts_at)
);
COMMENT ON TABLE billing.limit_windows IS 'One customer''s use of one limit in one window, identified by the server clock: a fixed, session or billing-period window, a rolling limit''s bucket, the one row of a lifetime or rate limit, or a billing policy''s spend window (spend:<identity>). Retention: rows are deleted 61 days after window_ends_at; a row without an end stays while its customer does.';
COMMENT ON COLUMN billing.limit_windows.limit_key IS 'A declared limit''s key, or spend: and the hashed identity of a billing policy''s spend window, which is a fixed window in the request currency on its hashed anchor.';
COMMENT ON COLUMN billing.limit_windows.window_starts_at IS 'The window''s start; 1970-01-01 for the one row of a lifetime or rate limit.';
COMMENT ON COLUMN billing.limit_windows.window_ends_at IS 'The window''s end; null for a lifetime or rate limit.';
COMMENT ON COLUMN billing.limit_windows.capacity IS 'The capacity products or an override gave the limit at the row''s last write, without adjustments. Reads take the live capacity.';
COMMENT ON COLUMN billing.limit_windows.settled IS 'Units settled in the window, never clamped: it may pass capacity. For a rate limit, the level as of updated_at, which drains at capacity per window_seconds.';
COMMENT ON COLUMN billing.limit_windows.adjusted IS 'Capacity staff added to this window only (negative takes it away).';
COMMENT ON COLUMN billing.limit_windows.reached_at IS 'When settled and held usage first took the window to its capacity; a rate or lifetime limit''s one row keeps the first time.';

CREATE INDEX limit_windows_window_ends_at_idx ON billing.limit_windows (merchant_id, window_ends_at)
    WHERE window_ends_at IS NOT NULL;

-- Limits are catalog objects with owned fields, and products own their limits.
ALTER TABLE billing.catalog_field_owners DROP CONSTRAINT catalog_field_owners_object_check;
ALTER TABLE billing.catalog_field_owners ADD CONSTRAINT catalog_field_owners_object_check
    CHECK (object IN ('product', 'price', 'meter', 'limit'));
ALTER TABLE billing.catalog_field_owners DROP CONSTRAINT catalog_field_owners_field_check;
ALTER TABLE billing.catalog_field_owners ADD CONSTRAINT catalog_field_owners_field_check CHECK (
    (object = 'product' AND field IN ('display_name', 'description', 'tier_group', 'tier_rank', 'archived', 'ownership', 'entitlements', 'required_entitlements', 'credit_grant', 'rate_cards', 'limits'))
    OR (object = 'price' AND field IN ('currency', 'unit_amount', 'access_duration_hours', 'billing_interval_hours', 'billing_interval_months', 'provider_access_default', 'trial_unit_amount', 'trial_duration_hours', 'customer_amount', 'quantity', 'archived', 'sell_via', 'provider'))
    OR (object = 'meter' AND field IN ('event_type', 'value_property', 'aggregation', 'unit', 'group_by'))
    OR (object = 'limit' AND field IN ('unit', 'reset', 'overrun', 'rate_cards'))
);
COMMENT ON COLUMN billing.catalog_field_owners.key IS 'The product, meter or limit key; a price''s product key.';
COMMENT ON COLUMN billing.catalog_field_owners.price_key IS 'A price''s key; empty for a product, meter or limit.';

-- A billing restore's destination holds none of the merchant's limits, holds
-- or leases.
CREATE OR REPLACE FUNCTION billing.guard_billing_restore_receipt() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
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
              -- Every merchant-scoped table but the directory's own identity rows
              -- (former names, host claims); a test keeps this list complete.
              AND c.relname = ANY(ARRAY[
                  'access_cutover_approvals',
                  'balance_watches',
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_limits',
                  'catalog_meters',
                  'catalog_product_limits',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'custodians',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customer_limit_overrides',
                  'customer_low_balance_lines',
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
                  'eventlog_events',
                  'failed_usage_windows',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'leases',
                  'ledger_accounts',
                  'ledger_transfers',
                  'limit_adjustments',
                  'limit_hold_parts',
                  'limit_holds',
                  'limit_windows',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'meter_period_quantities',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_events',
                  'order_attempts',
                  'order_lines',
                  'orders',
                  'owed_items',
                  'ownership_claims',
                  'payment_attempts',
                  'payment_method_versions',
                  'payment_methods',
                  'payments',
                  'price_key_movements',
                  'price_keys',
                  'price_migrations',
                  'price_psp_bindings',
                  'prices',
                  'product_access',
                  'product_entitlements',
                  'products',
                  'provider_bills',
                  'provider_charge_attributions',
                  'provider_cycle_receipts',
                  'provider_intents',
                  'provider_mutation_logs',
                  'provider_schedules',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'running_charges',
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'stored_card_checks',
                  'subscription_status_transitions',
                  'subscription_billing_changes',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_events',
                  'webhook_events',
                  'webhook_health',
                  'webhook_health_daily']::text[])
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
           OR OLD.id::text IS DISTINCT FROM current_setting('openrails.billing_restore_id',true)
           OR NEW.summary->>'digest' IS NULL OR NEW.summary->>'digest' !~ '^[0-9a-f]{64}$'
           OR jsonb_typeof(NEW.summary->'rows') IS DISTINCT FROM 'number'
           OR (NEW.summary->>'rows')::numeric < 0
           OR (NEW.summary->>'rows')::numeric <> trunc((NEW.summary->>'rows')::numeric) THEN
            RAISE EXCEPTION 'invalid billing restore receipts transition' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$_$;

-- Holds and leases. A hold sets capacity aside for one customer: one admitted
-- request owns it, or a gate's lease does. Holds replace admission_operations,
-- and denial statistics are gone: a limit window records when it was reached.


CREATE TABLE billing.leases (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_sync_at timestamptz NOT NULL,
    settled_through_n bigint NOT NULL CHECK (settled_through_n >= 0),
    closed_at timestamptz,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT leases_last_sync_at_check CHECK (last_sync_at >= created_at),
    CONSTRAINT leases_closed_at_check CHECK (closed_at IS NULL OR closed_at >= created_at)
);
COMMENT ON TABLE billing.leases IS 'One gate''s lease: a running process, or the processes that share one Redis. It is live while it is not closed and its gate synced within the lease lifetime; its holds live and die with it. Retention: rows are deleted 61 days after last_sync_at.';
COMMENT ON COLUMN billing.leases.id IS 'The UUID the client chose; lse_ on the wire.';
COMMENT ON COLUMN billing.leases.settled_through_n IS 'The highest usage item number settled. Items are numbered once per lease and settle in order.';

CREATE INDEX leases_last_sync_at_idx ON billing.leases (merchant_id, last_sync_at);

CREATE TABLE billing.limit_holds (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    created_at timestamptz NOT NULL DEFAULT now(),
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    lease_id uuid,
    limit_set text CHECK (octet_length(limit_set) BETWEEN 1 AND 255),
    request_id text CHECK (octet_length(request_id) BETWEEN 1 AND 255),
    estimated_amount bigint CHECK (estimated_amount >= 0),
    available_amount bigint CHECK (available_amount >= 0),
    terms jsonb CHECK (jsonb_typeof(terms) = 'object' AND octet_length(terms::text) <= 65536),
    estimate_usage jsonb,
    rate_cards jsonb,
    requested_expires_at timestamptz,
    expires_at timestamptz,
    state text NOT NULL CHECK (state IN ('open', 'released', 'captured')),
    capture_terms jsonb CHECK (jsonb_typeof(capture_terms) = 'object' AND octet_length(capture_terms::text) <= 65536),
    captured_amount bigint,
    captured_at timestamptz,
    released_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id, created_at),
    CONSTRAINT limit_holds_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT limit_holds_lease_id_fkey FOREIGN KEY (merchant_id, lease_id) REFERENCES billing.leases (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT limit_holds_owner_check CHECK (
        (request_id IS NOT NULL AND lease_id IS NULL AND limit_set IS NULL
            AND estimated_amount IS NOT NULL AND available_amount IS NOT NULL AND terms IS NOT NULL
            AND (estimate_usage IS NULL) = (rate_cards IS NULL))
        OR (lease_id IS NOT NULL AND limit_set IS NOT NULL AND request_id IS NULL
            AND estimated_amount IS NULL AND available_amount IS NULL AND terms IS NULL AND estimate_usage IS NULL
            AND requested_expires_at IS NULL AND expires_at IS NULL AND state <> 'captured')
    ),
    CONSTRAINT limit_holds_requested_expires_at_check CHECK (
        (requested_expires_at IS NULL OR requested_expires_at > created_at)
        AND (request_id IS NULL OR estimated_amount = 0 OR requested_expires_at IS NOT NULL)
    ),
    CONSTRAINT limit_holds_expires_at_check CHECK (requested_expires_at IS NULL OR (expires_at IS NOT NULL AND expires_at >= requested_expires_at)),
    CONSTRAINT limit_holds_hold_lifetime_check CHECK (expires_at IS NULL OR expires_at <= created_at + interval '720 hours'),
    CONSTRAINT limit_holds_state_fields_check CHECK (
        (state = 'open' AND capture_terms IS NULL AND captured_amount IS NULL AND captured_at IS NULL AND released_at IS NULL)
        OR (state = 'released' AND capture_terms IS NULL AND captured_amount IS NULL AND captured_at IS NULL AND released_at IS NOT NULL)
        OR (state = 'captured' AND capture_terms IS NOT NULL AND captured_amount IS NOT NULL AND captured_amount >= 0 AND captured_at IS NOT NULL)
    ),
    CONSTRAINT limit_holds_estimate_usage_check CHECK (
        (estimate_usage IS NULL OR (jsonb_typeof(estimate_usage) = 'object' AND octet_length(estimate_usage::text) <= 65536))
        AND (rate_cards IS NULL OR (jsonb_typeof(rate_cards) = 'array' AND octet_length(rate_cards::text) <= 262144))
    )
) PARTITION BY RANGE (created_at);
COMMENT ON TABLE billing.limit_holds IS 'Capacity set aside for one customer, in one currency: an admitted request''s hold (request_id, until it is captured, released or reaches expires_at) or a gate lease''s hold for one limit set (live while its lease is). What it holds of each limit is in limit_hold_parts. Retention: monthly partitions on created_at, dropped once older than the 61-day replay horizon.';
COMMENT ON COLUMN billing.limit_holds.created_at IS 'When the hold was issued; the partition key. A hold row is live at most 30 days past it: a request''s deadline is at most that, and a lease''s hold is issued again before then. request_id is unique per merchant among retained holds: admission serializes on it, because a partitioned key must carry created_at.';
COMMENT ON COLUMN billing.limit_holds.limit_set IS 'A lease''s hold: the opaque key of the limits that apply to its invokers after policy resolution; invokers with the same limit set share the hold.';
COMMENT ON COLUMN billing.limit_holds.estimated_amount IS 'A request''s hold: its money estimate, the host''s or what its usage estimate was priced at.';
COMMENT ON COLUMN billing.limit_holds.available_amount IS 'A request''s hold: what the customer could spend when it was admitted, before the hold.';
COMMENT ON COLUMN billing.limit_holds.estimate_usage IS 'The estimated usage ({event_type, dimensions, metadata, amount}); dynamic cards charge its optional amount. Null for a host-stated hold.';
COMMENT ON COLUMN billing.limit_holds.rate_cards IS 'The rate cards that priced the money estimate, pinned: the capture is priced by them, whatever the catalog says later.';
COMMENT ON COLUMN billing.limit_holds.expires_at IS 'A request''s deadline: past it the hold reserves no money and no end-less limit, and still counts in its windows until it is captured or released.';

CREATE INDEX limit_holds_request_id_idx ON billing.limit_holds (merchant_id, request_id, created_at DESC)
    WHERE request_id IS NOT NULL;
CREATE INDEX limit_holds_open_idx ON billing.limit_holds (merchant_id, customer_id, currency)
    WHERE state = 'open';
CREATE INDEX limit_holds_lease_id_idx ON billing.limit_holds (merchant_id, lease_id, customer_id, limit_set)
    WHERE lease_id IS NOT NULL;
CREATE INDEX limit_holds_created_at_request_id_idx ON billing.limit_holds (merchant_id, created_at DESC, request_id COLLATE "C" DESC)
    WHERE request_id IS NOT NULL;
CREATE INDEX limit_holds_customer_created_at_request_id_idx ON billing.limit_holds (merchant_id, customer_id, created_at DESC, request_id COLLATE "C" DESC)
    WHERE request_id IS NOT NULL;

CREATE TRIGGER immutable_request_hold_facts BEFORE DELETE OR UPDATE ON billing.limit_holds
    FOR EACH ROW WHEN (OLD.request_id IS NOT NULL)
    EXECUTE FUNCTION billing.guard_billing_fact_columns('expires_at', 'state', 'capture_terms', 'captured_amount', 'captured_at', 'released_at', 'updated_at');

CREATE TABLE billing.limit_hold_parts (
    merchant_id uuid NOT NULL,
    hold_id uuid NOT NULL,
    hold_created_at timestamptz NOT NULL,
    customer_id uuid NOT NULL,
    limit_key text NOT NULL CHECK (limit_key ~ '^[a-z][a-z0-9_-]{0,63}$' OR limit_key ~ '^spend:[0-9a-f]{64}$'),
    unit text NOT NULL CHECK (unit ~ '^[a-z][a-z0-9_]{0,63}$' OR unit ~ '^[A-Z0-9]{3,12}$'),
    window_starts_at timestamptz NOT NULL,
    window_ends_at timestamptz,
    held bigint NOT NULL CHECK (held >= 0),
    PRIMARY KEY (merchant_id, hold_id, limit_key, hold_created_at),
    CONSTRAINT limit_hold_parts_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT limit_hold_parts_window_check CHECK (window_ends_at IS NULL OR window_ends_at > window_starts_at)
) PARTITION BY RANGE (hold_created_at);
COMMENT ON TABLE billing.limit_hold_parts IS 'What one hold sets aside of one limit: the window it was issued in, which its capture settles into, and the units held there. balance has the Unix epoch and no end. A window''s held units are its live holds'' parts. Retention: monthly partitions on hold_created_at, dropped with their hold''s.';
COMMENT ON COLUMN billing.limit_hold_parts.hold_created_at IS 'The hold''s created_at: the partition key, shared with the hold so both drop together.';

CREATE INDEX limit_hold_parts_window_idx ON billing.limit_hold_parts (merchant_id, customer_id, limit_key, window_starts_at);

-- The migrator and the cleanup job keep the partitions current; these let
-- the tables take the retained admissions and new rows as soon as they exist.
SELECT billing.ensure_month_partitions('limit_holds', now() - interval '61 days', now() + interval '2 months');
SELECT billing.ensure_month_partitions('limit_hold_parts', now() - interval '61 days', now() + interval '2 months');

-- Retained admissions become request holds of their money; their spend
-- windows count afresh.
INSERT INTO billing.limit_holds (merchant_id, created_at, customer_id, currency, request_id, estimated_amount, available_amount,
    terms, estimate_usage, rate_cards, requested_expires_at, expires_at, state, capture_terms, captured_amount, captured_at, released_at, updated_at)
SELECT merchant_id, admitted_at, customer_id, currency, request_id, estimated_amount, available_amount,
    terms, estimate_usage, rate_cards, requested_expires_at, expires_at, state, capture_terms, captured_amount, captured_at, released_at,
    COALESCE(captured_at, released_at, admitted_at)
FROM billing.admission_operations
WHERE admitted_at >= now() - interval '61 days';
INSERT INTO billing.limit_hold_parts (merchant_id, hold_id, hold_created_at, customer_id, limit_key, unit, window_starts_at, held)
SELECT merchant_id, id, created_at, customer_id, 'balance', currency, 'epoch'::timestamptz, estimated_amount
FROM billing.limit_holds;

DROP TABLE billing.admission_denials_hourly;
DROP TABLE billing.admission_operations;

CREATE TABLE billing.limit_adjustments (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    source_id text NOT NULL CHECK (octet_length(source_id) BETWEEN 1 AND 255),
    limit_key text NOT NULL,
    amount bigint NOT NULL CHECK (amount <> 0),
    window_starts_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, source_id),
    CONSTRAINT limit_adjustments_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT limit_adjustments_limit_key_fkey FOREIGN KEY (merchant_id, limit_key) REFERENCES billing.catalog_limits (merchant_id, key) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.limit_adjustments IS 'One staff adjustment of a customer''s limit window, so a retry with its source_id adds nothing again; the capacity it added is the window''s adjusted. Retention: rows are deleted 61 days after created_at.';

CREATE INDEX limit_adjustments_created_at_idx ON billing.limit_adjustments (merchant_id, created_at);
CREATE INDEX limit_adjustments_limit_key_idx ON billing.limit_adjustments (merchant_id, limit_key);

-- Usage settles in batches. Raw events are gone: settlement keeps the
-- customer's daily accrual, which posts once to the ledger, a daily rollup
-- per meter for reports, and per-minute spend for rate measures.

-- A lease keeps the receipts of its latest settlements, so a resent item
-- answers its receipt and a changed one conflicts.
ALTER TABLE billing.leases ADD COLUMN receipts jsonb NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(receipts) = 'array' AND octet_length(receipts::text) <= 262144);
ALTER TABLE billing.leases ALTER COLUMN receipts DROP DEFAULT;
COMMENT ON COLUMN billing.leases.receipts IS 'The receipts of the lease''s latest settlements, oldest first: each one''s from_n, through_n, the digest of its items and the items it refused.';

-- A captured request keeps what each meter charged it, for its replay.
ALTER TABLE billing.limit_holds ADD COLUMN captured_charges jsonb
    CHECK (captured_charges IS NULL OR (state = 'captured' AND jsonb_typeof(captured_charges) = 'array'));
COMMENT ON COLUMN billing.limit_holds.captured_charges IS 'What each meter charged a captured request: [{meter_key, model, cell, quantity, amount}].';
DROP TRIGGER immutable_request_hold_facts ON billing.limit_holds;
CREATE TRIGGER immutable_request_hold_facts BEFORE DELETE OR UPDATE ON billing.limit_holds
    FOR EACH ROW WHEN (OLD.request_id IS NOT NULL)
    EXECUTE FUNCTION billing.guard_billing_fact_columns('expires_at', 'state', 'capture_terms', 'captured_amount', 'captured_at', 'captured_charges', 'released_at', 'updated_at');

-- A lease's hold is sized from its traffic: one request's worth, what it
-- used since the lease's previous sync, when another lease was refused for
-- capacity it holds, and the size its gate was last told.
ALTER TABLE billing.limit_hold_parts ADD COLUMN request_units bigint CHECK (request_units >= 0);
ALTER TABLE billing.limit_hold_parts ADD COLUMN used_units bigint CHECK (used_units >= 0);
ALTER TABLE billing.limit_hold_parts ADD COLUMN contended_at timestamptz;
ALTER TABLE billing.limit_hold_parts ADD COLUMN answered_units bigint CHECK (answered_units >= 0);
COMMENT ON COLUMN billing.limit_hold_parts.request_units IS 'A lease''s hold: what one request of its customer last cost in the limit; null for a request''s hold.';
COMMENT ON COLUMN billing.limit_hold_parts.used_units IS 'A lease''s hold: the units its usage took from the part since the lease''s previous sync; null for a request''s hold.';
COMMENT ON COLUMN billing.limit_hold_parts.answered_units IS 'A lease''s hold: the size its last sync answered, less what usage took since. A shrink is answered first and released at the next sync, down to this or what the gate''s open tickets took, whichever is more; null for a request''s hold.';
COMMENT ON COLUMN billing.limit_hold_parts.contended_at IS 'When another lease was refused capacity_held for capacity this part holds; it shrinks on its lease''s next sync.';

CREATE TABLE billing.usage_accruals (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    day_at timestamptz NOT NULL CHECK (day_at = date_trunc('day', day_at, 'UTC')),
    period_starts_at timestamptz NOT NULL,
    amount bigint NOT NULL CHECK (amount >= 0),
    posted_amount bigint NOT NULL CHECK (posted_amount >= 0),
    postings integer NOT NULL CHECK (postings >= 0),
    lines jsonb NOT NULL CHECK (jsonb_typeof(lines) = 'array' AND octet_length(lines::text) <= 262144),
    posted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT usage_accruals_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT usage_accruals_posted_amount_check CHECK (posted_amount <= amount),
    CONSTRAINT usage_accruals_posted_at_check CHECK ((posted_at IS NULL) = (postings = 0))
);
COMMENT ON TABLE billing.usage_accruals IS 'What one customer''s usage charged in one currency on one UTC day of one pricing period. It posts to the ledger after its day ends, or before any other movement of the customer''s balance in the currency: one credit_spend per lot drawn and one owed_accrual for the rest, at (usage_accrual, usage_accrual:<id>, <posting>). Until posted, the balance holds amount - posted_amount. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.usage_accruals.day_at IS 'The start of the UTC day the usage occurred.';
COMMENT ON COLUMN billing.usage_accruals.period_starts_at IS 'The start of the pricing period the usage was priced in.';
COMMENT ON COLUMN billing.usage_accruals.posted_amount IS 'What the accrual''s postings have moved to the ledger; the rest is held until the next posting.';
COMMENT ON COLUMN billing.usage_accruals.postings IS 'How many postings the accrual has made; the next one posts at source_id postings + 1.';
COMMENT ON COLUMN billing.usage_accruals.lines IS 'What the accrual is for, per meter and cell: [{meter_key, cell, requests, quantity, amount, posted_amount}].';

CREATE UNIQUE INDEX usage_accruals_day_at_key ON billing.usage_accruals (merchant_id, customer_id, currency, day_at, period_starts_at);
CREATE INDEX usage_accruals_due_idx ON billing.usage_accruals (day_at, merchant_id, customer_id, currency)
    WHERE amount > posted_amount;
CREATE INDEX usage_accruals_unposted_idx ON billing.usage_accruals (merchant_id, customer_id, currency)
    WHERE amount > posted_amount;

CREATE TABLE billing.usage_daily (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    day_at timestamptz NOT NULL CHECK (day_at = date_trunc('day', day_at, 'UTC')),
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    event_type text NOT NULL CHECK (octet_length(event_type) BETWEEN 1 AND 255),
    meter_key text NOT NULL,
    cell text NOT NULL,
    invoker text NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('succeeded', 'failed')),
    requests bigint NOT NULL CHECK (requests >= 0),
    quantity bigint NOT NULL CHECK (quantity >= 0),
    amount bigint NOT NULL CHECK (amount >= 0),
    forgiven_amount bigint NOT NULL CHECK (forgiven_amount >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, day_at, currency, event_type, meter_key, cell, invoker, outcome),
    CONSTRAINT usage_daily_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT usage_daily_meter_check CHECK (meter_key <> '' OR (cell = '' AND quantity = 0))
);
COMMENT ON TABLE billing.usage_daily IS 'One UTC day of a customer''s usage, per invoker, currency, event type, meter, matrix cell and outcome. The row with an empty meter_key counts the events themselves: requests, and what failed usage charged or forgave; a meter''s row counts the events it priced, their quantity and what it charged. Retention: rows are deleted 24 months after the month''s usage was billed.';
COMMENT ON COLUMN billing.usage_daily.meter_key IS 'The meter that priced the events; empty for the events themselves.';
COMMENT ON COLUMN billing.usage_daily.cell IS 'The matrix cell the meter priced the events in; empty without a matrix.';
COMMENT ON COLUMN billing.usage_daily.invoker IS 'Caller-supplied principal string that fired the usage; empty when none was named.';

CREATE INDEX usage_daily_day_at_idx ON billing.usage_daily (merchant_id, day_at);
CREATE INDEX usage_daily_event_type_day_at_idx ON billing.usage_daily (merchant_id, event_type, day_at);

CREATE TABLE billing.usage_minutes (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    minute timestamptz NOT NULL CHECK (minute = date_trunc('minute', minute)),
    amount bigint NOT NULL CHECK (amount >= 0),
    PRIMARY KEY (merchant_id, customer_id, currency, minute),
    CONSTRAINT usage_minutes_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.usage_minutes IS 'What a customer''s usage charged in one currency in one UTC minute it occurred in: the accrual-rate meter and the balance watch read it. Retention: rows are deleted 25 hours after their minute, past the longest accrual-rate window.';

CREATE INDEX usage_minutes_minute_idx ON billing.usage_minutes (merchant_id, minute);

-- Usage rollups replace raw events.
DROP TABLE billing.usage_events;

-- A billing restore's destination holds none of the merchant's usage either.
CREATE OR REPLACE FUNCTION billing.guard_billing_restore_receipt() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
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
              -- Every merchant-scoped table but the directory's own identity rows
              -- (former names, host claims); a test keeps this list complete.
              AND c.relname = ANY(ARRAY[
                  'access_cutover_approvals',
                  'balance_watches',
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_limits',
                  'catalog_meters',
                  'catalog_product_limits',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'custodians',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customer_limit_overrides',
                  'customer_low_balance_lines',
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
                  'eventlog_events',
                  'failed_usage_windows',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'leases',
                  'ledger_accounts',
                  'ledger_transfers',
                  'limit_adjustments',
                  'limit_hold_parts',
                  'limit_holds',
                  'limit_windows',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'meter_period_quantities',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_events',
                  'order_attempts',
                  'order_lines',
                  'orders',
                  'owed_items',
                  'ownership_claims',
                  'payment_attempts',
                  'payment_method_versions',
                  'payment_methods',
                  'payments',
                  'price_key_movements',
                  'price_keys',
                  'price_migrations',
                  'price_psp_bindings',
                  'prices',
                  'product_access',
                  'product_entitlements',
                  'products',
                  'provider_bills',
                  'provider_charge_attributions',
                  'provider_cycle_receipts',
                  'provider_intents',
                  'provider_mutation_logs',
                  'provider_schedules',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'running_charges',
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'stored_card_checks',
                  'subscription_status_transitions',
                  'subscription_billing_changes',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_accruals',
                  'usage_daily',
                  'usage_minutes',
                  'webhook_events',
                  'webhook_health',
                  'webhook_health_daily']::text[])
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
           OR OLD.id::text IS DISTINCT FROM current_setting('openrails.billing_restore_id',true)
           OR NEW.summary->>'digest' IS NULL OR NEW.summary->>'digest' !~ '^[0-9a-f]{64}$'
           OR jsonb_typeof(NEW.summary->'rows') IS DISTINCT FROM 'number'
           OR (NEW.summary->>'rows')::numeric < 0
           OR (NEW.summary->>'rows')::numeric <> trunc((NEW.summary->>'rows')::numeric) THEN
            RAISE EXCEPTION 'invalid billing restore receipts transition' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$_$;
