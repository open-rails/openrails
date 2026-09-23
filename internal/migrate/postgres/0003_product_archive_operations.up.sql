-- parent: 2 sha256:3892e7a8899a9df35e8f14ea58b1e50af424166e2cb0eba1bdd70da5057a4e56
-- Product archive operations (#1058): one immutable receipt per merchant
-- operation key. It fixes the resolved purchase window so replays evaluate the
-- same purchases; refunds and review findings carry their own identities.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.product_archive_operations (
    merchant_id uuid NOT NULL REFERENCES openrails.merchants(id) ON DELETE RESTRICT,
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
    FOREIGN KEY (merchant_id, product_id) REFERENCES openrails.products(merchant_id, id) ON DELETE RESTRICT,
    CHECK ((purchase_action = 'none') = (purchased_since IS NULL))
);
COMMENT ON TABLE openrails.product_archive_operations IS '#1058: immutable product archive receipts; the resolved purchase window and action are fixed at acceptance.';

CREATE FUNCTION openrails.guard_product_archive_operation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'product archive operations are immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER immutable_product_archive_operation BEFORE UPDATE OR DELETE ON openrails.product_archive_operations
 FOR EACH ROW EXECUTE FUNCTION openrails.guard_product_archive_operation();

-- A billing restore destination must also be empty of archive receipts.
CREATE OR REPLACE FUNCTION openrails.guard_billing_restore_receipt() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'openrails', 'pg_temp' AS $$
DECLARE item record; occupied boolean;
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.kind='billing_restore' THEN
            RAISE EXCEPTION 'billing restore receipts are immutable' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.kind<>'billing_restore' AND (TG_OP='INSERT' OR OLD.kind<>'billing_restore') THEN RETURN NEW; END IF;
    IF NEW.merchant_id IS DISTINCT FROM openrails.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    -- The merchant row is the serialization point used by begin_billing_restore
    -- and by FK-backed first writes. No database-owner exemption or GUC-only
    -- permission can create a receipt for an occupied destination.
    PERFORM 1 FROM openrails.merchants WHERE id=NEW.merchant_id AND status='active' AND deleted_at IS NULL FOR UPDATE;
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
           OR NOT EXISTS (SELECT 1 FROM openrails.maintenance_runs r WHERE r.id=OLD.id AND r.merchant_id=OLD.merchant_id
               AND r.xmin=pg_current_xact_id_if_assigned()::xid)
           OR OLD.id::text IS DISTINCT FROM current_setting('app.billing_restore_id',true)
           OR NEW.summary->>'digest' IS NULL OR NEW.summary->>'digest' !~ '^[0-9a-f]{64}$'
           OR jsonb_typeof(NEW.summary->'rows') IS DISTINCT FROM 'number'
           OR (NEW.summary->>'rows')::numeric < 0
           OR (NEW.summary->>'rows')::numeric <> trunc((NEW.summary->>'rows')::numeric) THEN
            RAISE EXCEPTION 'invalid billing restore receipts transition' USING ERRCODE='23514';
        END IF;
        PERFORM openrails.check_billing_restore_ledger(NEW.merchant_id);
    END IF;
    RETURN NEW;
END;
$$;
