-- parent: 17 sha256:b65eaace52b7ec0595bfdc5db66f5d7fa50b283e907b24ae6f2b749cc267394b
-- Repair: none-needed product_entitlements is created and filled in this file
-- before products.entitlements, the only source it copies, is dropped.

-- A product's keys with valid-time history. An edit closes or opens rows; it
-- never rewrites one, so a past instant answers with the keys then in force.
CREATE TABLE billing.product_entitlements (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    entitlement text COLLATE "C" NOT NULL,
    added_at timestamp with time zone NOT NULL,
    removed_at timestamp with time zone,
    added_by text NOT NULL,
    removed_by text,
    CONSTRAINT product_entitlements_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT product_entitlements_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT product_entitlements_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products(merchant_id, id),
    CONSTRAINT product_entitlements_entitlement_check CHECK (octet_length(entitlement) BETWEEN 1 AND 256 AND btrim(entitlement) <> ''),
    CONSTRAINT product_entitlements_window_check CHECK (removed_at IS NULL OR removed_at >= added_at),
    CONSTRAINT product_entitlements_removed_check CHECK ((removed_at IS NULL) = (removed_by IS NULL)),
    CONSTRAINT product_entitlements_added_by_check CHECK (octet_length(added_by) BETWEEN 1 AND 255),
    CONSTRAINT product_entitlements_removed_by_check CHECK (removed_by IS NULL OR octet_length(removed_by) BETWEEN 1 AND 255)
);
COMMENT ON TABLE billing.product_entitlements IS 'The entitlement keys each product grants, with valid-time history: a key holds for [added_at, removed_at). Holders derive their keys from these rows at check time. Rows are never deleted; removal closes a row. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.product_entitlements.added_at IS 'When the key started being granted. Keys that predate this history (migrated or restored from older archives) hold from 0001-01-01.';
COMMENT ON COLUMN billing.product_entitlements.added_by IS 'Who added the key: a catalog application id (sha256:...), an operator, or migration.';
COMMENT ON COLUMN billing.product_entitlements.removed_at IS 'When the key stopped being granted; equal to added_at for a key added and removed at the same instant.';

-- When a key was added before this history began is unknown: it holds from
-- the start of time, so imported purchases older than their product derive it.
INSERT INTO billing.product_entitlements (merchant_id, product_id, entitlement, added_at, added_by)
SELECT DISTINCT p.merchant_id, p.id, k.name, '0001-01-01 00:00:00+00'::timestamptz, 'migration'
FROM billing.products p CROSS JOIN LATERAL jsonb_array_elements_text(p.entitlements) AS k(name);

CREATE UNIQUE INDEX product_entitlements_live_key ON billing.product_entitlements (merchant_id, product_id, entitlement) WHERE removed_at IS NULL;
CREATE INDEX product_entitlements_product_idx ON billing.product_entitlements (merchant_id, product_id, entitlement, added_at) INCLUDE (removed_at);
CREATE INDEX product_entitlements_entitlement_idx ON billing.product_entitlements (merchant_id, entitlement, product_id) INCLUDE (added_at, removed_at);
COMMENT ON INDEX billing.product_entitlements_product_idx IS 'One product''s keys in byte order: prefix reads probe it once per held product.';
COMMENT ON INDEX billing.product_entitlements_entitlement_idx IS 'The products granting a key: exact checks, reverse lookups and ReplaceEntitlements.';

CREATE FUNCTION billing.guard_product_entitlement_history() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'product entitlement history is immutable' USING ERRCODE='23514';
    END IF;
    IF OLD.removed_at IS NOT NULL OR NEW.removed_at IS NULL
       OR (to_jsonb(NEW)-ARRAY['removed_at','removed_by']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['removed_at','removed_by']) THEN
        RAISE EXCEPTION 'a product entitlement row only closes, once' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER immutable_product_entitlement_history BEFORE UPDATE OR DELETE ON billing.product_entitlements
FOR EACH ROW EXECUTE FUNCTION billing.guard_product_entitlement_history();
CREATE TRIGGER immutable_product_entitlement_history_truncate BEFORE TRUNCATE ON billing.product_entitlements
FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- Key edits are catalog edits: one revision step per statement and merchant,
-- the way catalog_authored_write steps it per row of the other catalog tables.
CREATE FUNCTION billing.catalog_authored_entitlements() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE mid uuid;
BEGIN
    FOR mid IN SELECT DISTINCT merchant_id FROM changed LOOP
        IF current_setting('openrails.catalog_batch_merchant_id',true) IS DISTINCT FROM mid::text THEN
            BEGIN
                PERFORM id FROM billing.merchants WHERE id=mid FOR UPDATE NOWAIT;
            EXCEPTION WHEN lock_not_available THEN
                RAISE EXCEPTION 'concurrent catalog authoring; retry the transaction' USING ERRCODE='40001';
            END;
            UPDATE billing.merchants SET catalog_revision=catalog_revision+1 WHERE id=mid;
        END IF;
    END LOOP;
    RETURN NULL;
END $$;
CREATE TRIGGER catalog_authored_entitlements_insert AFTER INSERT ON billing.product_entitlements
REFERENCING NEW TABLE AS changed FOR EACH STATEMENT EXECUTE FUNCTION billing.catalog_authored_entitlements();
CREATE TRIGGER catalog_authored_entitlements_update AFTER UPDATE ON billing.product_entitlements
REFERENCING NEW TABLE AS changed FOR EACH STATEMENT EXECUTE FUNCTION billing.catalog_authored_entitlements();

-- A key edit is a product change: the writer steps the revision explicitly.
CREATE OR REPLACE FUNCTION billing.assign_product_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        NEW.revision := COALESCE(NEW.revision, 0);
    ELSIF NEW.revision IS DISTINCT FROM OLD.revision
       OR (to_jsonb(NEW)-ARRAY['updated_at','revision']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['updated_at','revision']) THEN
        NEW.revision := OLD.revision + 1;
    ELSE
        NEW.revision := OLD.revision;
    END IF;
    RETURN NEW;
END;
$$;

DROP INDEX billing.products_entitlements_idx;
ALTER TABLE billing.products DROP CONSTRAINT products_entitlements_list_check;
ALTER TABLE billing.products DROP COLUMN entitlements;

-- Product keys are part of the merchant book and occupy a restore destination.
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
