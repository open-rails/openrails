-- parent: 2 sha256:e970f356c77aa8c7a740f63ad7e159dde7c7ad7465e66fc079f143aa7ec611fd
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '300s';

-- Authored catalog revisions and durable bulk-operation receipts. Provider
-- observations are deliberately outside this revision domain.
ALTER TABLE openrails.merchants ADD COLUMN catalog_revision bigint NOT NULL DEFAULT 0 CHECK (catalog_revision >= 0);
CREATE TABLE openrails.catalog_applications (
    merchant_id uuid NOT NULL REFERENCES openrails.merchants(id) ON DELETE RESTRICT,
    application_id text NOT NULL CHECK (length(application_id) BETWEEN 1 AND 128),
    catalog_id uuid NOT NULL,
    schema_version bigint NOT NULL,
    request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256)=32),
    base_revision bigint NOT NULL CHECK (base_revision >= 0),
    applied_revision bigint NOT NULL CHECK (applied_revision = base_revision + 1),
    result jsonb NOT NULL CHECK (octet_length(result::text) <= 16384),
    applied_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id,application_id),
    FOREIGN KEY (merchant_id,catalog_id) REFERENCES openrails.catalogs(merchant_id,id) ON DELETE RESTRICT
);
COMMENT ON TABLE openrails.catalog_applications IS 'Permanent compact replay receipts, retained and restored with the merchant billing book; never expire by HTTP idempotency TTL.';

-- Individual services acquire the merchant lock first. This trigger also
-- fences direct imports/module writes, keeping them visible to application CAS.
CREATE FUNCTION openrails.catalog_authored_write() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE mid uuid;
BEGIN
    IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
    IF TG_OP='DELETE' THEN mid := OLD.merchant_id; ELSE mid := NEW.merchant_id; END IF;
    IF current_setting('app.catalog_batch',true) IS DISTINCT FROM mid::text THEN
        -- A legacy raw writer may already hold a child-row lock. Do not wait
        -- behind a merchant-first transaction while holding that child: refuse
        -- with a retryable serialization conflict instead of deadlocking.
        BEGIN
            PERFORM id FROM openrails.merchants WHERE id=mid FOR UPDATE NOWAIT;
        EXCEPTION WHEN lock_not_available THEN
            RAISE EXCEPTION 'concurrent catalog authoring; retry the transaction' USING ERRCODE='40001';
        END;
        UPDATE openrails.merchants SET catalog_revision=catalog_revision+1 WHERE id=mid;
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END $$;
CREATE TRIGGER catalog_authored_product BEFORE INSERT OR UPDATE OR DELETE ON openrails.products FOR EACH ROW EXECUTE FUNCTION openrails.catalog_authored_write();
CREATE TRIGGER catalog_authored_price BEFORE INSERT OR UPDATE OR DELETE ON openrails.prices FOR EACH ROW EXECUTE FUNCTION openrails.catalog_authored_write();
CREATE TRIGGER catalog_authored_catalog BEFORE INSERT OR UPDATE OR DELETE ON openrails.catalogs FOR EACH ROW EXECUTE FUNCTION openrails.catalog_authored_write();
CREATE TRIGGER catalog_authored_binding BEFORE INSERT OR UPDATE OR DELETE ON openrails.price_psp_bindings FOR EACH ROW EXECUTE FUNCTION openrails.catalog_authored_write();
CREATE TRIGGER catalog_authored_meter BEFORE INSERT OR UPDATE OR DELETE ON openrails.catalog_meters FOR EACH ROW EXECUTE FUNCTION openrails.catalog_authored_write();
CREATE TRIGGER catalog_authored_rate_card BEFORE INSERT OR UPDATE OR DELETE ON openrails.catalog_rate_cards FOR EACH ROW EXECUTE FUNCTION openrails.catalog_authored_write();

-- An unassigned pointer is a real historical state, not the previous active
-- offer. UUIDv7 movement IDs give deterministic tie ordering within a timestamp.
ALTER TABLE openrails.price_key_movements ADD COLUMN archived boolean NOT NULL DEFAULT false;

CREATE FUNCTION openrails.catalog_price_retired() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT OLD.archived AND (NEW.archived OR NEW.key <> OLD.key) THEN
  INSERT INTO openrails.price_key_movements(merchant_id,key,price_id,effective_at,archived) VALUES(OLD.merchant_id,OLD.key,OLD.id,clock_timestamp(),true);
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER catalog_price_retired AFTER UPDATE OF archived,key ON openrails.prices FOR EACH ROW EXECUTE FUNCTION openrails.catalog_price_retired();

-- Product eligibility is a separate lifecycle flag. Its changes affect offers
-- without changing their own archive flags or repricing existing subscribers.
CREATE FUNCTION openrails.catalog_product_offer_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.archived IS DISTINCT FROM OLD.archived THEN
  INSERT INTO openrails.price_key_movements(merchant_id,key,price_id,effective_at,archived)
   SELECT p.merchant_id,p.key,p.id,clock_timestamp(),NEW.archived FROM openrails.prices p
   WHERE p.merchant_id=NEW.merchant_id AND p.product_id=NEW.id AND NOT p.archived;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER catalog_product_offer_state AFTER UPDATE OF archived ON openrails.products FOR EACH ROW EXECUTE FUNCTION openrails.catalog_product_offer_state();

CREATE FUNCTION openrails.guard_catalog_application_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'catalog application receipts are immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER immutable_catalog_application_receipt BEFORE UPDATE OR DELETE ON openrails.catalog_applications FOR EACH ROW EXECUTE FUNCTION openrails.guard_catalog_application_receipt();

-- Restore occupancy includes the permanent application ledger.
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
