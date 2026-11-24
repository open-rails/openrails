-- Gates decide on views: a lease owns no capacity, only the request holds
-- its gate admitted near zero and the report of what it has in flight.

DELETE FROM billing.limit_hold_parts p USING billing.limit_holds h
WHERE p.merchant_id = h.merchant_id AND p.hold_id = h.id AND p.hold_created_at = h.created_at
  AND h.request_id IS NULL;
DELETE FROM billing.limit_holds WHERE request_id IS NULL;

DROP INDEX billing.limit_holds_lease_id_idx;
ALTER TABLE billing.limit_holds DROP CONSTRAINT limit_holds_owner_check;
ALTER TABLE billing.limit_holds DROP CONSTRAINT limit_holds_version_check;
ALTER TABLE billing.limit_holds DROP CONSTRAINT limit_holds_requested_expires_at_check;
-- A lease's hold row outlives its lease when retention deletes the lease
-- first; readers count it only while the lease is live.
ALTER TABLE billing.limit_holds DROP CONSTRAINT limit_holds_lease_id_fkey;
ALTER TABLE billing.limit_holds DROP COLUMN limit_set;
ALTER TABLE billing.limit_holds DROP COLUMN version;
ALTER TABLE billing.limit_holds DROP COLUMN applied_version;
ALTER TABLE billing.limit_holds ALTER COLUMN request_id SET NOT NULL;
ALTER TABLE billing.limit_holds ALTER COLUMN estimated_amount SET NOT NULL;
ALTER TABLE billing.limit_holds ALTER COLUMN available_amount SET NOT NULL;
ALTER TABLE billing.limit_holds ALTER COLUMN terms SET NOT NULL;
ALTER TABLE billing.limit_holds ADD CONSTRAINT limit_holds_rate_cards_check CHECK ((estimate_usage IS NULL) = (rate_cards IS NULL));
ALTER TABLE billing.limit_holds ADD CONSTRAINT limit_holds_lease_id_check CHECK (lease_id IS NULL OR (requested_expires_at IS NULL AND expires_at IS NULL));
ALTER TABLE billing.limit_holds ADD CONSTRAINT limit_holds_requested_expires_at_check CHECK (
    (requested_expires_at IS NULL OR requested_expires_at > created_at)
    AND (lease_id IS NOT NULL OR estimated_amount = 0 OR requested_expires_at IS NOT NULL)
);
CREATE INDEX limit_holds_lease_id_idx ON billing.limit_holds (merchant_id, lease_id)
    WHERE lease_id IS NOT NULL AND state = 'open';
COMMENT ON TABLE billing.limit_holds IS 'Capacity set aside for one admitted request of one customer, in one currency, until it is captured, released or reaches expires_at; a gate''s hold (lease_id) has no deadline and counts while its lease is live. What it holds of each limit is in limit_hold_parts. Retention: monthly partitions on created_at, dropped once older than the 61-day replay horizon.';
COMMENT ON COLUMN billing.limit_holds.lease_id IS 'The gate lease that admitted the request near its limit: the hold counts while the lease is live, is captured by the lease''s usage and released by its sync or close.';
COMMENT ON COLUMN billing.limit_holds.terms IS 'The admission terms: {invoker, invoker_type, trust_level, roles, resource, source, accrual_rate_delta_per_hour}.';
COMMENT ON COLUMN billing.limit_holds.created_at IS 'When the hold was issued; the partition key. A hold row is live at most 30 days past it. request_id is unique per merchant among retained holds: admission serializes on it, because a partitioned key must carry created_at.';

ALTER TABLE billing.limit_hold_parts DROP COLUMN request_units;
ALTER TABLE billing.limit_hold_parts DROP COLUMN used_units;
ALTER TABLE billing.limit_hold_parts DROP COLUMN contended_at;
ALTER TABLE billing.limit_hold_parts DROP COLUMN answered_units;

ALTER TABLE billing.leases DROP COLUMN hold_version;
COMMENT ON TABLE billing.leases IS 'One gate''s lease, or a batch job''s usage stream: its numbered usage, its in-flight report and the request holds its gate admitted. It is live while it is not closed and synced within the lease lifetime; a later sync makes it live again. Retention: rows are deleted 61 days after last_sync_at.';
COMMENT ON COLUMN billing.leases.closed_at IS 'When a sync with close ended the lease, released its request holds and cleared its in-flight report. Usage against it still settles.';

CREATE TABLE billing.lease_in_flight (
    merchant_id uuid NOT NULL,
    lease_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    limit_key text NOT NULL CHECK (limit_key ~ '^[a-z][a-z0-9_-]{0,63}$' OR limit_key ~ '^spend:[0-9a-f]{64}$'),
    unit text NOT NULL CHECK (unit ~ '^[a-z][a-z0-9_]{0,63}$' OR unit ~ '^[A-Z0-9]{3,12}$'),
    amount bigint NOT NULL CHECK (amount > 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, limit_key, unit, lease_id),
    CONSTRAINT lease_in_flight_lease_id_fkey FOREIGN KEY (merchant_id, lease_id) REFERENCES billing.leases (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT lease_in_flight_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.lease_in_flight IS 'What a gate has had in flight for one customer''s limit across at least one sync: open tickets and usage a sync left unsettled. Other gates'' views and admissions count it as held in the limit''s current window while its lease is live. Retention: rows; each sync replaces its lease''s rows, close clears them, and they go with the lease.';
COMMENT ON COLUMN billing.lease_in_flight.limit_key IS 'balance, a declared limit''s key, or a spend window''s spend:<identity>.';
COMMENT ON COLUMN billing.lease_in_flight.unit IS 'The limit''s unit: balance''s currency.';

CREATE INDEX lease_in_flight_lease_id_idx ON billing.lease_in_flight (merchant_id, lease_id);

-- The restore guard names the new table.
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
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
                  'disputes',
                  'eventlog_events',
                  'failed_usage_windows',
                  'fraud_reports',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'lease_in_flight',
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
