-- parent: 67 sha256:f8429901c74af88f5794886cef10b195af6379bd0f09500213af40dae97d6992
-- Repair: none-needed The new table and column hold no stored row a check refuses: existing events record no charges, and a metered card's payment term is cleared before its check.
-- Usage is priced when it is recorded: each meter's period-to-date quantity
-- per customer prices the next event at the difference it makes, so the
-- period-close sweep and its watermarks go.

CREATE TABLE billing.meter_period_quantities (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    meter_key text NOT NULL,
    cell text NOT NULL,
    period_starts_at timestamp with time zone NOT NULL,
    quantity bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT meter_period_quantities_pkey PRIMARY KEY (merchant_id, customer_id, currency, meter_key, cell, period_starts_at),
    CONSTRAINT meter_period_quantities_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT meter_period_quantities_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT meter_period_quantities_currency_check CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    CONSTRAINT meter_period_quantities_meter_key_check CHECK (meter_key <> ''),
    CONSTRAINT meter_period_quantities_quantity_check CHECK (quantity >= 0)
);
COMMENT ON TABLE billing.meter_period_quantities IS 'A customer''s quantity of one meter (and matrix cell) in one billing period, as priced so far: an event is charged price(quantity + its own) - price(quantity), then added. Retention: rows are deleted 100 days after period_starts_at.';
COMMENT ON COLUMN billing.meter_period_quantities.cell IS 'The matrix cell (the event''s value of the matrix dimension) the quantity is priced in; empty for a card without a matrix.';
COMMENT ON COLUMN billing.meter_period_quantities.period_starts_at IS 'Start of the billing period the usage occurred in, under the merchant''s billing_period_boundary.';

CREATE INDEX meter_period_quantities_period_starts_at_idx ON billing.meter_period_quantities USING btree (merchant_id, period_starts_at);

DROP TABLE billing.metered_rating_watermarks;

-- What each meter charged the event, posted with it.
ALTER TABLE billing.usage_events ADD COLUMN charges jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE billing.usage_events ALTER COLUMN charges DROP DEFAULT;
ALTER TABLE billing.usage_events ADD CONSTRAINT usage_events_charges_check CHECK (jsonb_typeof(charges) = 'array');
COMMENT ON COLUMN billing.usage_events.charges IS 'What each meter charged the event when it was recorded: [{meter_key, cell, quantity, amount}]; empty for an event no priced meter consumes and for a host-priced event.';
COMMENT ON COLUMN billing.usage_events.pricing_authority IS 'host = amount is the host''s final price; catalog = the meters'' rate cards priced the event when it was recorded (charges). Capture and failed usage write host.';
COMMENT ON TABLE billing.usage_events IS 'Append-only multi-dimensional metered usage. Source of truth for usage reporting and usage order lines. Priced when recorded, by the host (amount) or the rate cards (charges); event and ledger postings commit in one tx. The hot admission path never reads this table. Retention: monthly partitions on occurred_at, dropped 24 months after the month''s usage was billed.';

-- A metered card's charge is collected as the customer's billing mode says;
-- only a flat fee states when it is collected.
ALTER TABLE billing.catalog_rate_cards ALTER COLUMN payment_term DROP NOT NULL;
UPDATE billing.catalog_rate_cards SET payment_term = NULL WHERE meter_key IS NOT NULL;
ALTER TABLE billing.catalog_rate_cards ADD CONSTRAINT catalog_rate_cards_payment_term_metered_check CHECK ((meter_key IS NULL) = (payment_term IS NOT NULL));
COMMENT ON COLUMN billing.catalog_rate_cards.payment_term IS 'When a flat fee is collected; null for a metered card, whose charges are posted when usage is recorded.';

-- The rate cards an event is priced by are read by its event type.
CREATE INDEX catalog_meters_event_type_idx ON billing.catalog_meters USING btree (merchant_id, (COALESCE(NULLIF(event_type, ''), key)));

-- The counters are merchant rows and occupy a restore destination.
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
                  'cost_observations',
                  'cost_qualifications',
                  'cost_refusals',
                  'cost_resolutions',
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
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
                  'notifications',
                  'operation_authorization_extensions',
                  'operation_authorizations',
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
