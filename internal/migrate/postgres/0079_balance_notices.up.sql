-- Repair: none-needed The new columns start null or zero, and the backfill writes only rows it creates.
-- Every prepaid balance is watched, not only a running charge's: a null
-- next_check_at is a watch nothing has made due.
ALTER TABLE billing.balance_watches
    ALTER COLUMN next_check_at DROP NOT NULL,
    ADD COLUMN funded_at timestamp with time zone,
    ADD COLUMN usage_rate_per_hour bigint NOT NULL DEFAULT 0,
    ADD COLUMN rate_sampled_at timestamp with time zone,
    ADD CONSTRAINT balance_watches_usage_rate_per_hour_check CHECK (usage_rate_per_hour >= 0);
ALTER TABLE billing.balance_watches ALTER COLUMN usage_rate_per_hour DROP DEFAULT;

DROP INDEX billing.balance_watches_next_check_at_idx;
CREATE INDEX balance_watches_next_check_at_idx ON billing.balance_watches USING btree (next_check_at, merchant_id, customer_id, currency)
    WHERE next_check_at IS NOT NULL;

INSERT INTO billing.balance_watches (merchant_id, customer_id, currency, next_check_at, usage_rate_per_hour, updated_at)
SELECT a.merchant_id, a.customer_id, a.currency, NULL, 0, now()
FROM billing.ledger_accounts a
LEFT JOIN billing.money_settings s
  ON s.merchant_id = a.merchant_id AND s.customer_id = a.customer_id AND s.currency = a.currency
WHERE a.account_type = 'customer_balance' AND a.customer_id IS NOT NULL
  AND (s.billing_mode IS DISTINCT FROM 'arrears' OR s.collection_revoked_at IS NOT NULL)
ON CONFLICT (merchant_id, customer_id, currency) DO NOTHING;

COMMENT ON TABLE billing.balance_watches IS 'One row per prepaid customer balance and per customer and currency with a running charge: when its balance is next looked at (a low line, the run-out, a credit lot lapse or the next 00:00 UTC, whichever is first; null until a change marks it due) and what the last look found. Derived from the balance, the running charges and the billing mode; an arrears customer with no running charge has none.';
COMMENT ON COLUMN billing.balance_watches.next_check_at IS 'When the balance is next looked at; to_timestamp(0) once a change marks it due, null with nothing scheduled.';
COMMENT ON COLUMN billing.balance_watches.depletes_at IS 'When the customer''s capacity reaches zero at the rate of its running charges and the lapse of its credit lots, as last computed; in the past while it is at zero, null when nothing exact moves it.';
COMMENT ON COLUMN billing.balance_watches.low_notified_at IS 'When balance.low was sent for the current crossing; cleared by a look that finds the balance above its line after a funding.';
COMMENT ON COLUMN billing.balance_watches.depleted_at IS 'When balance.depleted was sent for the current crossing; cleared by a look that finds capacity above zero after a funding.';
COMMENT ON COLUMN billing.balance_watches.funded_at IS 'When the customer was funded (a deposit, a credit grant, a reinstatement or a payment of what it owes) since its last notice; only funding re-arms a notice, and a notice clears it.';
COMMENT ON COLUMN billing.balance_watches.usage_rate_per_hour IS 'Exponentially weighted rate of usage spend, one-hour time constant, in native units per hour: each look adds the usage events dated since rate_sampled_at.';
COMMENT ON COLUMN billing.balance_watches.rate_sampled_at IS 'The instant usage_rate_per_hour was sampled at; usage events dated before it are not read again.';

-- A customer's own low-balance line in a currency, apart from its money
-- settings so that setting one decides no billing mode.
CREATE TABLE billing.customer_low_balance_lines (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    amount bigint,
    runway_hours integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT customer_low_balance_lines_pkey PRIMARY KEY (merchant_id, customer_id, currency),
    CONSTRAINT customer_low_balance_lines_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT customer_low_balance_lines_currency_check CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    CONSTRAINT customer_low_balance_lines_amount_check CHECK (amount >= 0),
    CONSTRAINT customer_low_balance_lines_runway_hours_check CHECK (runway_hours >= 0 AND runway_hours <= 8760),
    CONSTRAINT customer_low_balance_lines_set_check CHECK (amount IS NOT NULL OR runway_hours IS NOT NULL)
);
COMMENT ON TABLE billing.customer_low_balance_lines IS 'A customer''s own low-balance line per currency: the amount line and the runway line (hours of spend at its burn rate). A null part inherits the merchant''s settings.balance_notices; a line with no part set has no row.';

-- A customer's line names one more merchant table a restore must find empty.
CREATE OR REPLACE FUNCTION billing.guard_billing_restore_receipt() RETURNS trigger
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
              -- Every merchant-scoped table but the directory's own identity rows
              -- (former names, host claims); a test keeps this list complete.
              AND c.relname = ANY(ARRAY[
                  'access_cutover_approvals',
                  'account_updater_batches',
                  'admission_denials_hourly',
                  'admission_operations',
                  'balance_watches',
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_meters',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'checkout_attempts',
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customer_low_balance_lines',
                  'customer_links',
                  'customers',
                  'dashboard_configs',
                  'destructive_run_before_images',
                  'document_sequences',
                  'failed_usage_windows',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'ledger_accounts',
                  'ledger_transfers',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'meter_period_quantities',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_event_acks',
                  'notification_events',
                  'notifications',
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
                  'provider_intents',
                  'provider_mutation_logs',
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
                  'subscription_status_transitions',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_collection_cadence',
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
        PERFORM billing.check_billing_restore_ledger(NEW.merchant_id);
    END IF;
    RETURN NEW;
END;
$$;


-- The customer's line replaces the running charge's.
ALTER TABLE billing.running_charges DROP COLUMN low_balance_amount;

COMMENT ON TABLE billing.host_outbox IS 'Typed durable host events: successful rail payment settlements, delinquency lifecycle transitions, product key changes, order transitions, customer links, revoked charge agreements and the low and depleted balance of a watched customer. Acknowledge after idempotent processing; acknowledgments are separate from notification read state. Retention: delivered events are deleted 30 days after delivered_at; an undelivered event is never deleted.';
