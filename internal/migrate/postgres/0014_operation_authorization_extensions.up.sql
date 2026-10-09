-- parent: 13 sha256:a47f61861f81b8f9e850d65930d625621ab1ad28304ad6d7b4cfd356f09f57ba
-- Repair: none-needed No authorization has been extended: extended_amount starts
-- at 0, so every existing authorized_amount equals its opening amount.
ALTER TABLE billing.operation_authorizations
    ADD COLUMN extended_amount bigint DEFAULT 0 NOT NULL,
    ADD COLUMN authorized_amount bigint NOT NULL GENERATED ALWAYS AS (amount + extended_amount) STORED;
ALTER TABLE billing.operation_authorizations
    ADD CONSTRAINT operation_authorizations_extended_amount_check CHECK ((extended_amount >= 0));
COMMENT ON COLUMN billing.operation_authorizations.extended_amount IS 'Sum of granted_amount over the authorization''s extensions; grows only while open.';
COMMENT ON COLUMN billing.operation_authorizations.authorized_amount IS 'The hold: the opening amount plus every extension grant.';

DROP TRIGGER immutable_operation_authorization_facts ON billing.operation_authorizations;
CREATE TRIGGER immutable_operation_authorization_facts BEFORE UPDATE OR DELETE ON billing.operation_authorizations
FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('state','terminal_reference','released_at','settled_at','settlement_cost_amount','settlement_amount','settlement_body_bytes','settlement_body_digest','extended_amount','authorized_amount');

CREATE TABLE billing.operation_authorization_extensions (
    merchant_id uuid NOT NULL,
    operation_id text NOT NULL,
    ordinal bigint NOT NULL,
    requested_amount bigint NOT NULL,
    minimum_amount bigint NOT NULL,
    granted_amount bigint NOT NULL,
    authorized_amount bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT operation_authorization_extensions_ordinal_check CHECK ((ordinal >= 1)),
    CONSTRAINT operation_authorization_extensions_amounts_check CHECK (((minimum_amount > 0) AND (minimum_amount <= granted_amount) AND (granted_amount <= requested_amount))),
    CONSTRAINT operation_authorization_extensions_authorized_check CHECK ((authorized_amount > granted_amount))
);
COMMENT ON TABLE billing.operation_authorization_extensions IS 'Each granted growth of an open operation authorization''s hold, gapless by ordinal from 1: what the host asked for, the least it accepted, what was granted, and the authorized total after the grant. Immutable. Retention: permanent, never pruned.';

ALTER TABLE ONLY billing.operation_authorization_extensions
    ADD CONSTRAINT operation_authorization_extensions_pkey PRIMARY KEY (merchant_id, operation_id, ordinal);
ALTER TABLE ONLY billing.operation_authorization_extensions
    ADD CONSTRAINT operation_authorization_extensions_authorization_fkey FOREIGN KEY (merchant_id, operation_id) REFERENCES billing.operation_authorizations(merchant_id, operation_id) ON DELETE RESTRICT;

CREATE TRIGGER immutable_operation_authorization_extensions BEFORE UPDATE OR DELETE ON billing.operation_authorization_extensions
FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- Extensions are merchant rows and occupy a restore destination.
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
                  'credential_publications',
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_invoice_profiles',
                  'customers',
                  'dashboard_configs',
                  'destructive_run_before_images',
                  'entitlements',
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
                  'price_psp_bindings',
                  'prices',
                  'product_archive_operations',
                  'products',
                  'provider_intents',
                  'provider_mutation_logs',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'reprice_batches',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'subscription_reprices',
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
