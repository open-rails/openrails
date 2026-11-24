-- Customer messages: what OpenRails sends a customer about each billing fact,
-- one row per fact and channel, queued in the transaction that records it.

CREATE TABLE billing.customer_messages (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    customer_id uuid NOT NULL,
    event_id uuid NOT NULL,
    event_type text NOT NULL CHECK (octet_length(event_type) BETWEEN 1 AND 255),
    channel text NOT NULL CHECK (channel IN ('email')),
    status text NOT NULL CHECK (status IN ('pending', 'queued', 'sending', 'sent', 'disabled', 'test_environment', 'no_contact', 'rate_limited', 'failed')),
    attempts integer NOT NULL CHECK (attempts >= 0),
    last_error_code text CHECK (last_error_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    recipient text CHECK (octet_length(recipient) BETWEEN 3 AND 320),
    sent_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT customer_messages_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT customer_messages_event_id_channel_key UNIQUE (merchant_id, event_id, channel),
    CONSTRAINT customer_messages_sent_check CHECK ((status = 'sent') = (sent_at IS NOT NULL)),
    CONSTRAINT customer_messages_addressed_check CHECK (status NOT IN ('sending', 'sent') OR recipient IS NOT NULL)
);
COMMENT ON TABLE billing.customer_messages IS 'One message to a customer about one billing fact (a notification_events row) on one channel, and what became of it. No body is kept: it is the delivery log a dispute response cites. A fact sends at most one message per channel. Retention: rows are deleted two years (731 days) after created_at.';
COMMENT ON COLUMN billing.customer_messages.event_id IS 'The notification_events id of the fact; kept after the event itself is pruned.';
COMMENT ON COLUMN billing.customer_messages.status IS 'pending until a dispatcher queues its send job, queued until the job settles it, sending while the job hands it to the sender; then sent, or why not: disabled (the merchant does not send this event type), test_environment (its PSP moves no real money), no_contact (no address), rate_limited (the customer''s hourly cap) or failed.';
COMMENT ON COLUMN billing.customer_messages.attempts IS 'Send attempts the job has made.';
COMMENT ON COLUMN billing.customer_messages.last_error_code IS 'Why the last attempt did not send; NULL after a send.';
COMMENT ON COLUMN billing.customer_messages.recipient IS 'The address the message was handed to.';
COMMENT ON COLUMN billing.customer_messages.sent_at IS 'When the sender accepted the message.';

CREATE INDEX customer_messages_pending_idx ON billing.customer_messages (created_at, merchant_id, id)
    WHERE status = 'pending';
CREATE INDEX customer_messages_customer_idx ON billing.customer_messages (merchant_id, customer_id, created_at, id);
CREATE INDEX customer_messages_created_at_idx ON billing.customer_messages (merchant_id, created_at, id);

-- Every fact queues its message as it commits; a billing restore sends
-- nothing again. The notification only wakes a dispatcher: the payload is
-- the schema, so a long schema name never makes the channel too long.
CREATE FUNCTION billing.queue_customer_message() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    INSERT INTO billing.customer_messages (merchant_id, customer_id, event_id, event_type, channel, status, attempts)
    VALUES (NEW.merchant_id, NEW.customer_id, NEW.id, NEW.event_type, 'email', 'pending', 0)
    ON CONFLICT (merchant_id, event_id, channel) DO NOTHING;
    IF FOUND THEN
        PERFORM pg_notify('openrails_customer_messages', TG_TABLE_SCHEMA);
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER notification_event_customer_message AFTER INSERT ON billing.notification_events
    FOR EACH ROW EXECUTE FUNCTION billing.queue_customer_message();

-- A billing restore's destination holds none of the merchant's messages.
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
                  'custodians',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customer_limit_overrides',
                  'customer_low_balance_lines',
                  'customer_messages',
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
