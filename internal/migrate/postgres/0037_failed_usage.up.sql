-- parent: 36 sha256:127cecfe2584035e274ee63cb1f3e988c18f09b7b8a37e8f64f7090d4d63be0d
-- Repair: none-needed Every existing usage event succeeded and forgave nothing, which the new checks accept; the defaults fill only the existing rows.
-- Usage that failed is a usage event with outcome failed: what the failure
-- cost, forgiven up to the customer's grace and charged past it. Its amount is
-- what it charged, forgiven_amount what grace absorbed, and it is final: never
-- rated by the catalog. Every writer supplies both columns.
ALTER TABLE billing.usage_events
    ADD COLUMN outcome text NOT NULL DEFAULT 'succeeded',
    ADD COLUMN forgiven_amount bigint NOT NULL DEFAULT 0;
ALTER TABLE billing.usage_events
    ALTER COLUMN outcome DROP DEFAULT,
    ALTER COLUMN forgiven_amount DROP DEFAULT;
ALTER TABLE billing.usage_events
    ADD CONSTRAINT usage_events_outcome_check CHECK ((outcome = ANY (ARRAY['succeeded'::text, 'failed'::text]))),
    ADD CONSTRAINT usage_events_forgiven_amount_check CHECK (((forgiven_amount >= 0) AND ((outcome = 'failed'::text) OR (forgiven_amount = 0)))),
    ADD CONSTRAINT usage_events_failed_host_priced_check CHECK (((outcome = 'succeeded'::text) OR (pricing_authority = 'host'::text)));
COMMENT ON COLUMN billing.usage_events.outcome IS 'succeeded, or failed: work that cost the platform and did not deliver. A failed event charges only what the customer''s grace leaves.';
COMMENT ON COLUMN billing.usage_events.forgiven_amount IS 'The part of a failed event''s reported cost its grace absorbed; amount + forgiven_amount is what the host reported. Zero for a succeeded event.';

-- The failed-usage windows: what failed per customer (invoker empty, its
-- grace) or per delegated invoker (its cutoff), per configured window. They
-- were Redis counters; here they commit with the usage event that fills them.
CREATE TABLE billing.failed_usage_windows (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    invoker text NOT NULL,
    window_key text NOT NULL,
    window_start timestamp with time zone NOT NULL,
    window_end timestamp with time zone NOT NULL,
    amount bigint NOT NULL,
    CONSTRAINT failed_usage_windows_pkey PRIMARY KEY (merchant_id, customer_id, currency, invoker, window_key, window_start),
    CONSTRAINT failed_usage_windows_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT failed_usage_windows_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT failed_usage_windows_amount_check CHECK ((amount > 0)),
    CONSTRAINT failed_usage_windows_currency_check CHECK ((currency ~ '^[A-Z0-9]{3,12}$'::text)),
    CONSTRAINT failed_usage_windows_invoker_check CHECK ((octet_length(invoker) <= 255)),
    CONSTRAINT failed_usage_windows_window_key_check CHECK (((window_key <> ''::text) AND (octet_length(window_key) <= 255))),
    CONSTRAINT failed_usage_windows_window_check CHECK ((window_end > window_start))
);
COMMENT ON TABLE billing.failed_usage_windows IS 'Failed usage counted per customer and configured window: the customer''s grace (invoker empty) or a delegated invoker''s cutoff. Retention: rows are deleted once their window has ended.';
COMMENT ON COLUMN billing.failed_usage_windows.invoker IS 'Empty for the customer''s own grace window; otherwise the delegated invoker the cutoff window meters.';

CREATE INDEX failed_usage_windows_window_end_idx ON billing.failed_usage_windows USING btree (merchant_id, window_end);

-- Failed-usage windows are merchant rows and occupy a restore destination.
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
                  'billing_policies',
                  'billing_policy_bindings',
                  'card_attempt_failures',
                  'catalog_applications',
                  'catalog_meters',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'checkout_attempts',
                  'checkout_sessions',
                  'cost_observations',
                  'cost_qualifications',
                  'cost_refusals',
                  'cost_resolutions',
                  'credential_publications',
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customers',
                  'dashboard_configs',
                  'failed_usage_windows',
                  'destructive_run_before_images',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'invoice_collection_cadence',
                  'invoice_items',
                  'invoice_payments',
                  'invoices',
                  'invoker_spend_limits',
                  'ledger_accounts',
                  'ledger_transfers',
                  'maintenance_runs',
                  'mandates',
                  'merchant_configuration_applications',
                  'merchant_configurations',
                  'merchant_deks',
                  'merchant_destructive_policy',
                  'merchant_secrets',
                  'merchant_webhooks',
                  'metered_rating_watermarks',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notifications',
                  'operation_authorization_extensions',
                  'operation_authorizations',
                  'payment_attempts',
                  'payment_method_updates',
                  'payment_methods',
                  'payments',
                  'price_key_movements',
                  'price_migrations',
                  'price_psp_bindings',
                  'prices',
                  'product_archive_operations',
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
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'subscription_status_transitions',
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
        PERFORM billing.check_billing_restore_ledger(NEW.merchant_id);
    END IF;
    RETURN NEW;
END;
$$;

