-- Revenue schedules: how the general ledger recognises each paid order line's
-- deferred revenue, at month boundaries in the merchant's accounting timezone
-- (a setting in its configuration, never stored here).

CREATE TABLE billing.revenue_schedules (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    order_id uuid NOT NULL,
    order_line_id uuid NOT NULL,
    subscription_id uuid,
    product_access_id uuid,
    product_id uuid,
    price_id uuid,
    method text NOT NULL CHECK (method IN ('ratable', 'point_in_time')),
    revenue_account text NOT NULL CHECK (revenue_account IN ('subscription_revenue', 'one_time_revenue')),
    funded bigint NOT NULL,
    recognized_net bigint NOT NULL,
    service_starts_at timestamptz NOT NULL,
    service_ends_at timestamptz NOT NULL,
    service_months integer CHECK (service_months BETWEEN 1 AND 1200),
    ended_at timestamptz,
    recognized_through_at timestamptz NOT NULL,
    next_recognition_at timestamptz,
    reinstatements integer NOT NULL CHECK (reinstatements >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT revenue_schedules_order_line_id_key UNIQUE (merchant_id, order_line_id),
    CONSTRAINT revenue_schedules_order_id_fkey FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders (merchant_id, customer_id, id),
    CONSTRAINT revenue_schedules_order_line_id_fkey FOREIGN KEY (merchant_id, order_line_id) REFERENCES billing.order_lines (merchant_id, id),
    CONSTRAINT revenue_schedules_subscription_id_fkey FOREIGN KEY (merchant_id, customer_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, customer_id, id),
    CONSTRAINT revenue_schedules_product_access_id_fkey FOREIGN KEY (merchant_id, product_access_id) REFERENCES billing.product_access (merchant_id, id),
    CONSTRAINT revenue_schedules_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id),
    CONSTRAINT revenue_schedules_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices (merchant_id, id),
    CONSTRAINT revenue_schedules_funded_check CHECK (0 <= recognized_net AND recognized_net <= funded),
    CONSTRAINT revenue_schedules_service_check CHECK (
        method = 'ratable' AND service_ends_at > service_starts_at
        OR method = 'point_in_time' AND service_ends_at = service_starts_at AND service_months IS NULL),
    CONSTRAINT revenue_schedules_recognized_through_check CHECK (recognized_through_at >= service_starts_at AND recognized_through_at <= service_ends_at),
    CONSTRAINT revenue_schedules_ended_check CHECK (ended_at >= service_starts_at AND ended_at < service_ends_at)
);
COMMENT ON TABLE billing.revenue_schedules IS 'One row per paid order line whose revenue the general ledger defers: what was funded (credited to deferred_revenue), what has been recognised, and over what service. A ratable schedule recognises by elapsed service at every midnight on the 1st in the merchant''s accounting timezone and at its end; a point-in-time one at once. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.revenue_schedules.revenue_account IS 'The general-ledger revenue account recognition credits.';
COMMENT ON COLUMN billing.revenue_schedules.funded IS 'The net transaction price allocated to the line: cash applied and non-cash settlement, less later reductions.';
COMMENT ON COLUMN billing.revenue_schedules.recognized_net IS 'What recognition has moved from deferred_revenue to revenue_account, less refund catch-ups.';
COMMENT ON COLUMN billing.revenue_schedules.service_months IS 'For service sold in calendar months: their number. Each month recognises an equal share, linear within it; NULL recognises by elapsed time over the whole service.';
COMMENT ON COLUMN billing.revenue_schedules.ended_at IS 'When the service ended before service_ends_at (an immediate cancellation, a revocation): what remains unrecognised then is forfeited to revenue.';
COMMENT ON COLUMN billing.revenue_schedules.recognized_through_at IS 'The instant recognition has reached: the application, or the last boundary posted.';
COMMENT ON COLUMN billing.revenue_schedules.reinstatements IS 'How many forfeits an undone end took back: the generation of the schedule''s termination and reinstatement coordinates, so an end repeated at one instant posts again.';
COMMENT ON COLUMN billing.revenue_schedules.next_recognition_at IS 'When the schedule is next due to the revenue_recognition job; NULL once fully recognised. An early end makes it due then.';

CREATE INDEX revenue_schedules_due_idx ON billing.revenue_schedules (next_recognition_at, merchant_id, id)
    WHERE next_recognition_at IS NOT NULL;
CREATE INDEX revenue_schedules_subscription_id_idx ON billing.revenue_schedules (merchant_id, subscription_id)
    WHERE subscription_id IS NOT NULL;
CREATE INDEX revenue_schedules_product_access_id_idx ON billing.revenue_schedules (merchant_id, product_access_id)
    WHERE product_access_id IS NOT NULL;

CREATE TRIGGER immutable_revenue_schedule_facts BEFORE DELETE OR UPDATE ON billing.revenue_schedules
    FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('funded', 'recognized_net', 'ended_at', 'recognized_through_at', 'next_recognition_at', 'reinstatements', 'updated_at');
CREATE TRIGGER immutable_revenue_schedules_truncate BEFORE TRUNCATE ON billing.revenue_schedules
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- A general-ledger transfer names the schedule whose deferred revenue it moves.
ALTER TABLE billing.ledger_transfers
    ADD COLUMN revenue_schedule_id uuid,
    ADD CONSTRAINT ledger_transfers_revenue_schedule_check CHECK (book = 'general' OR revenue_schedule_id IS NULL);
COMMENT ON COLUMN billing.ledger_transfers.revenue_schedule_id IS 'The revenue schedule whose deferred revenue a general-ledger transfer funds, recognises or reduces. No foreign key (ledger purity).';

-- A service that ends early ends its schedules there: they fall due then, and
-- the recognition job forfeits what remains. An end undone or moved later
-- (a reactivation, a destructive run's undo) makes them due at the old end, and
-- the job takes back what it forfeited and recognises on. A replaced
-- subscription's schedules (an upgrade) keep running.
CREATE FUNCTION billing.end_revenue_schedules() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    ended timestamptz;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NULL; END IF;
    IF TG_TABLE_NAME = 'subscriptions' THEN
        ended := CASE WHEN NEW.cancel_type IS DISTINCT FROM 'upgrade' THEN NEW.ended_at END;
        UPDATE billing.revenue_schedules s
           SET ended_at = CASE WHEN ended < s.service_ends_at THEN GREATEST(ended, s.service_starts_at) END,
               next_recognition_at = LEAST(s.next_recognition_at, s.ended_at), updated_at = now()
         WHERE s.merchant_id = NEW.merchant_id AND s.subscription_id = NEW.id
           AND s.ended_at IS NOT NULL AND (ended IS NULL OR ended > s.ended_at);
        UPDATE billing.revenue_schedules s
           SET ended_at = GREATEST(ended, s.service_starts_at),
               next_recognition_at = LEAST(s.next_recognition_at, GREATEST(ended, s.service_starts_at)), updated_at = now()
         WHERE s.merchant_id = NEW.merchant_id AND s.subscription_id = NEW.id
           AND s.next_recognition_at IS NOT NULL AND ended < s.service_ends_at
           AND (s.ended_at IS NULL OR ended < s.ended_at);
        RETURN NULL;
    END IF;
    ended := LEAST(COALESCE(NEW.revoked_at, 'infinity'), COALESCE(NEW.deleted_at, 'infinity'), COALESCE(NEW.ends_at, 'infinity'));
    UPDATE billing.revenue_schedules s
       SET ended_at = CASE WHEN ended < s.service_ends_at THEN GREATEST(ended, s.service_starts_at) END,
           next_recognition_at = LEAST(s.next_recognition_at, s.ended_at), updated_at = now()
     WHERE s.merchant_id = NEW.merchant_id AND s.product_access_id = NEW.id
       AND s.ended_at IS NOT NULL AND ended > s.ended_at;
    UPDATE billing.revenue_schedules s
       SET ended_at = GREATEST(ended, s.service_starts_at),
           next_recognition_at = LEAST(s.next_recognition_at, GREATEST(ended, s.service_starts_at)), updated_at = now()
     WHERE s.merchant_id = NEW.merchant_id AND s.product_access_id = NEW.id
       AND s.next_recognition_at IS NOT NULL AND ended < s.service_ends_at
       AND (s.ended_at IS NULL OR ended < s.ended_at);
    RETURN NULL;
END;
$$;

CREATE TRIGGER subscriptions_end_revenue_schedules AFTER UPDATE OF ended_at, cancel_type ON billing.subscriptions
    FOR EACH ROW WHEN (NEW.ended_at IS DISTINCT FROM OLD.ended_at OR NEW.cancel_type IS DISTINCT FROM OLD.cancel_type)
    EXECUTE FUNCTION billing.end_revenue_schedules();
CREATE TRIGGER product_access_end_revenue_schedules AFTER UPDATE OF revoked_at, deleted_at, ends_at ON billing.product_access
    FOR EACH ROW WHEN (NEW.revoked_at IS DISTINCT FROM OLD.revoked_at OR NEW.deleted_at IS DISTINCT FROM OLD.deleted_at OR NEW.ends_at IS DISTINCT FROM OLD.ends_at)
    EXECUTE FUNCTION billing.end_revenue_schedules();

-- A billing restore's destination holds none of the merchant's schedules.
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
                  'balance_watches',
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_limits',
                  'catalog_meters',
                  'catalog_product_limits',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'credit_grant_events',
                  'credit_grants',
                  'custodians',
                  'customer_delinquency',
                  'customer_invoice_profiles',
                  'customer_limit_overrides',
                  'customer_low_balance_lines',
                  'customer_messages',
                  'customer_mrr',
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
                  'disputes',
                  'eventlog_events',
                  'fraud_reports',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'lease_in_flight',
                  'leases',
                  'ledger_accounts',
                  'ledger_daily',
                  'ledger_rollup_pending',
                  'ledger_transfers',
                  'ledger_type_totals',
                  'limit_adjustments',
                  'limit_hold_parts',
                  'limit_holds',
                  'limit_windows',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'money_settings',
                  'mrr_cohort_flows',
                  'mrr_flows',
                  'mrr_movement_lines',
                  'mrr_movements',
                  'mrr_product_flows',
                  'mrr_scheduled',
                  'mrr_totals',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_events',
                  'operation_log',
                  'order_attempts',
                  'order_lines',
                  'orders',
                  'owed_items',
                  'ownership_claims',
                  'payment_attempts',
                  'payment_method_versions',
                  'payment_methods',
                  'payments',
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
                  'provider_schedules',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'revenue_schedules',
                  'running_charges',
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'stored_card_checks',
                  'subscription_billing_changes',
                  'subscription_mrr',
                  'subscription_status_transitions',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_card_prints',
                  'usage_postings',
                  'usage_unposted',
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
