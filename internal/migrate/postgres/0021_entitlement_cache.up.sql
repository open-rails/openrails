-- parent: 20 sha256:5a4a32abdefb11fdbc156f7344586478fa99cad4892d332678b163deabdf5e76
-- Repair: none-needed The new columns default to zero, and the new tables start
-- empty; no constraint is added over stored rows.

-- A heavy buyer's keys, cached. A customer holding many products derives keys
-- from all of them; the cache answers prefix reads and key pages from one
-- range. An entry is valid only while both stamps match and the instant asked
-- is inside its window: the merchant's entitlement generation (any key edit
-- steps it) and the customer's access version (any change to their product
-- access steps it). Readers check both in the snapshot they read the keys in,
-- so the cache never answers stale.
ALTER TABLE billing.merchants ADD COLUMN entitlement_generation bigint DEFAULT 0 NOT NULL;
COMMENT ON COLUMN billing.merchants.entitlement_generation IS 'Steps with every statement that changes a product''s keys: a cached key set from an older generation is stale.';
ALTER TABLE billing.customers ADD COLUMN access_version bigint DEFAULT 0 NOT NULL;
COMMENT ON COLUMN billing.customers.access_version IS 'Steps with every statement that changes the customer''s product access: a cached key set from an older version is stale.';

CREATE FUNCTION billing.step_entitlement_generation() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE mid uuid;
BEGIN
    FOR mid IN SELECT DISTINCT merchant_id FROM changed LOOP
        UPDATE billing.merchants SET entitlement_generation = entitlement_generation + 1 WHERE id = mid;
    END LOOP;
    RETURN NULL;
END $$;
CREATE TRIGGER product_entitlements_step_generation_insert AFTER INSERT ON billing.product_entitlements
REFERENCING NEW TABLE AS changed FOR EACH STATEMENT EXECUTE FUNCTION billing.step_entitlement_generation();
CREATE TRIGGER product_entitlements_step_generation_update AFTER UPDATE ON billing.product_entitlements
REFERENCING NEW TABLE AS changed FOR EACH STATEMENT EXECUTE FUNCTION billing.step_entitlement_generation();

-- Customers in id order, so concurrent writers lock them in one order.
CREATE FUNCTION billing.step_access_version() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE mid uuid; cid uuid;
BEGIN
    FOR mid, cid IN SELECT DISTINCT merchant_id, customer_id FROM changed ORDER BY merchant_id, customer_id LOOP
        UPDATE billing.customers SET access_version = access_version + 1 WHERE merchant_id = mid AND id = cid;
    END LOOP;
    RETURN NULL;
END $$;
CREATE TRIGGER product_access_step_version_insert AFTER INSERT ON billing.product_access
REFERENCING NEW TABLE AS changed FOR EACH STATEMENT EXECUTE FUNCTION billing.step_access_version();
CREATE TRIGGER product_access_step_version_update AFTER UPDATE ON billing.product_access
REFERENCING NEW TABLE AS changed FOR EACH STATEMENT EXECUTE FUNCTION billing.step_access_version();
CREATE TRIGGER product_access_step_version_delete AFTER DELETE ON billing.product_access
REFERENCING OLD TABLE AS changed FOR EACH STATEMENT EXECUTE FUNCTION billing.step_access_version();

-- A prefix read probes each held product's keys. With the product a key
-- column of the key index, the planner could instead skip-scan a key range
-- once per held product: O(held products x keys in range). Product is only
-- carried, so the per-product index is the one way in.
DROP INDEX billing.product_entitlements_entitlement_idx;
CREATE INDEX product_entitlements_entitlement_idx ON billing.product_entitlements (merchant_id, entitlement) INCLUDE (product_id, added_at, removed_at);
COMMENT ON INDEX billing.product_entitlements_entitlement_idx IS 'The products granting a key: exact checks, reverse lookups and ReplaceEntitlements.';

CREATE TABLE billing.customer_entitlement_cache_stamps (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    entitlement_generation bigint NOT NULL,
    access_version bigint NOT NULL,
    valid_from timestamp with time zone NOT NULL,
    valid_until timestamp with time zone,
    keys integer NOT NULL,
    held_products integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT customer_entitlement_cache_stamps_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT customer_entitlement_cache_stamps_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id),
    CONSTRAINT customer_entitlement_cache_stamps_window_check CHECK (valid_until IS NULL OR valid_from < valid_until),
    CONSTRAINT customer_entitlement_cache_stamps_counts_check CHECK (keys >= 0 AND held_products >= 0)
);
CREATE UNIQUE INDEX customer_entitlement_cache_stamps_customer_key ON billing.customer_entitlement_cache_stamps (merchant_id, customer_id);
COMMENT ON TABLE billing.customer_entitlement_cache_stamps IS 'When a heavy buyer''s cached keys are valid: built at entitlement_generation and access_version, for instants in [valid_from, valid_until), the next start or end of one of their windows. Derived; rebuilt on read.';

CREATE TABLE billing.customer_entitlement_cache (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    entitlement text COLLATE "C" NOT NULL,
    CONSTRAINT customer_entitlement_cache_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT customer_entitlement_cache_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id)
);
CREATE UNIQUE INDEX customer_entitlement_cache_key ON billing.customer_entitlement_cache (merchant_id, customer_id, entitlement);
COMMENT ON TABLE billing.customer_entitlement_cache IS 'A heavy buyer''s keys as of their stamps row: the keys of the products they hold. Derived; rebuilt on read.';

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
                  'credential_publications',
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customers',
                  'dashboard_configs',
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
