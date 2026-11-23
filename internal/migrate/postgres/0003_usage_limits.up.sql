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
    limit_key text NOT NULL CHECK (limit_key ~ '^[a-z][a-z0-9_-]{0,63}$'),
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
COMMENT ON TABLE billing.limit_windows IS 'One customer''s use of one limit in one window, identified by the server clock: a fixed, session or billing-period window, a rolling limit''s bucket, or the one row of a lifetime or rate limit. Retention: rows are deleted 61 days after window_ends_at; a row without an end stays while its customer does.';
COMMENT ON COLUMN billing.limit_windows.window_starts_at IS 'The window''s start; 1970-01-01 for the one row of a lifetime or rate limit.';
COMMENT ON COLUMN billing.limit_windows.window_ends_at IS 'The window''s end; null for a lifetime or rate limit.';
COMMENT ON COLUMN billing.limit_windows.capacity IS 'The capacity products or an override gave the limit at the row''s last write, without adjustments. Reads take the live capacity.';
COMMENT ON COLUMN billing.limit_windows.settled IS 'Units settled in the window, never clamped: it may pass capacity. For a rate limit, the level as of updated_at, which drains at capacity per window_seconds.';
COMMENT ON COLUMN billing.limit_windows.adjusted IS 'Capacity staff added to this window only (negative takes it away).';
COMMENT ON COLUMN billing.limit_windows.reached_at IS 'When settled and held usage first took the window to its capacity.';

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

-- A billing restore's destination holds none of the merchant's limits.
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
                  'admission_denials_hourly',
                  'admission_operations',
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
                  'ledger_accounts',
                  'ledger_transfers',
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
