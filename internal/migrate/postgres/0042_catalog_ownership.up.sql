-- parent: 41 sha256:3520284944cff2914942ed9cd2ace776e1efee46ef3d2e33d62576ae24f49eb8
-- Repair: none-needed Existing products shift their revision up by one so revisions start at 1; existing meters, rate cards and price keys start at 1; no field has an owner until a document or an edit sets it.
-- Catalog field ownership, as server-side apply: a document (the apply
-- manager) and individual edits (the edit manager) record which fields they
-- set; a document skips an object whose field an edit set differently.
-- Every product, price key, meter and rate card carries a revision that
-- advances on each change.

CREATE TABLE billing.catalog_field_owners (
    merchant_id uuid NOT NULL,
    object text NOT NULL,
    key text NOT NULL,
    price_key text NOT NULL,
    field text NOT NULL,
    manager text NOT NULL,
    actor text NOT NULL,
    set_at timestamp with time zone NOT NULL,
    CONSTRAINT catalog_field_owners_pkey PRIMARY KEY (merchant_id, object, key, price_key, field, manager),
    CONSTRAINT catalog_field_owners_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT catalog_field_owners_object_check CHECK ((object = ANY (ARRAY['product'::text, 'price'::text, 'meter'::text]))),
    CONSTRAINT catalog_field_owners_key_check CHECK (((key <> ''::text) AND (octet_length(key) <= 255))),
    CONSTRAINT catalog_field_owners_price_key_check CHECK ((((object = 'price'::text) = (price_key <> ''::text)) AND (octet_length(price_key) <= 255))),
    CONSTRAINT catalog_field_owners_field_check CHECK ((
        ((object = 'product'::text) AND (field = ANY (ARRAY['display_name'::text, 'description'::text, 'tier_group'::text, 'tier_rank'::text, 'archived'::text, 'ownership'::text, 'entitlements'::text, 'credit_grant'::text, 'rate_cards'::text])))
        OR ((object = 'price'::text) AND (field = ANY (ARRAY['currency'::text, 'unit_amount'::text, 'access_duration_hours'::text, 'billing_interval_hours'::text, 'trial_unit_amount'::text, 'trial_duration_hours'::text, 'customer_amount'::text, 'quantity'::text, 'archived'::text, 'psp_links'::text])))
        OR ((object = 'meter'::text) AND (field = ANY (ARRAY['event_type'::text, 'value_property'::text, 'aggregation'::text, 'unit'::text, 'group_by'::text]))))),
    CONSTRAINT catalog_field_owners_manager_check CHECK ((manager = ANY (ARRAY['apply'::text, 'edit'::text]))),
    CONSTRAINT catalog_field_owners_actor_check CHECK (((actor <> ''::text) AND (octet_length(actor) <= 255)))
);
COMMENT ON TABLE billing.catalog_field_owners IS 'Which manager set each catalog field: apply (a catalog document) or edit (an individual edit), and who and when. Both own a field set to an equal value. A document skips an object whose field it names an edit set differently.';
COMMENT ON COLUMN billing.catalog_field_owners.key IS 'The product or meter key; a price''s product key.';
COMMENT ON COLUMN billing.catalog_field_owners.price_key IS 'A price''s key; empty for a product or meter.';
COMMENT ON COLUMN billing.catalog_field_owners.actor IS 'apply: the document''s application id; edit: the signed-in user, or api.';

-- One revision rule for every catalog object row: 1 on insert unless given
-- (a restore), then one step per change or explicit step.
CREATE FUNCTION billing.assign_object_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        NEW.revision := COALESCE(NEW.revision, 1);
    ELSIF NEW.revision IS DISTINCT FROM OLD.revision
       OR (to_jsonb(NEW)-ARRAY['updated_at','revision']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['updated_at','revision']) THEN
        NEW.revision := OLD.revision + 1;
    ELSE
        NEW.revision := OLD.revision;
    END IF;
    RETURN NEW;
END $$;

ALTER TABLE billing.products ALTER COLUMN revision DROP DEFAULT;
ALTER TABLE billing.products DROP CONSTRAINT products_revision_check;
UPDATE billing.products SET revision = revision + 1;
ALTER TABLE billing.products ADD CONSTRAINT products_revision_check CHECK ((revision >= 1));
DROP TRIGGER product_revision ON billing.products;
DROP FUNCTION billing.assign_product_revision();
CREATE TRIGGER product_revision BEFORE INSERT OR UPDATE ON billing.products
FOR EACH ROW EXECUTE FUNCTION billing.assign_object_revision();
COMMENT ON COLUMN billing.products.revision IS 'Advances on every change to the product, its keys or its rate cards; never on a change to its prices.';

ALTER TABLE billing.catalog_meters ADD COLUMN revision bigint;
UPDATE billing.catalog_meters SET revision = 1;
ALTER TABLE billing.catalog_meters ALTER COLUMN revision SET NOT NULL;
ALTER TABLE billing.catalog_meters ADD CONSTRAINT catalog_meters_revision_check CHECK ((revision >= 1));
CREATE TRIGGER catalog_meter_revision BEFORE INSERT OR UPDATE ON billing.catalog_meters
FOR EACH ROW EXECUTE FUNCTION billing.assign_object_revision();
COMMENT ON COLUMN billing.catalog_meters.revision IS 'Advances on every change to the meter or its rate card.';

ALTER TABLE billing.catalog_rate_cards ADD COLUMN revision bigint;
UPDATE billing.catalog_rate_cards SET revision = 1;
ALTER TABLE billing.catalog_rate_cards ALTER COLUMN revision SET NOT NULL;
ALTER TABLE billing.catalog_rate_cards ADD CONSTRAINT catalog_rate_cards_revision_check CHECK ((revision >= 1));
CREATE TRIGGER catalog_rate_card_revision BEFORE INSERT OR UPDATE ON billing.catalog_rate_cards
FOR EACH ROW EXECUTE FUNCTION billing.assign_object_revision();
COMMENT ON COLUMN billing.catalog_rate_cards.revision IS 'Advances on every change to the card: a rate override''s revision.';

-- A price key is one catalog object across its versions and their PSP links.
CREATE TABLE billing.price_keys (
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    key text NOT NULL,
    revision bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT price_keys_pkey PRIMARY KEY (merchant_id, product_id, key),
    CONSTRAINT price_keys_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products(merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT price_keys_revision_check CHECK ((revision >= 1))
);
COMMENT ON TABLE billing.price_keys IS 'One row per price key of a product: its revision advances on every change to any of its versions or their PSP links. Written only by the prices and price_psp_bindings triggers, which rebuild it on restore.';

INSERT INTO billing.price_keys (merchant_id, product_id, key, revision)
SELECT DISTINCT merchant_id, product_id, key, 1 FROM billing.prices;

CREATE FUNCTION billing.step_price_key() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP='UPDATE' AND (to_jsonb(NEW)-ARRAY['updated_at']) IS NOT DISTINCT FROM (to_jsonb(OLD)-ARRAY['updated_at']) THEN
        RETURN NULL;
    END IF;
    INSERT INTO billing.price_keys AS k (merchant_id, product_id, key, revision)
    VALUES (NEW.merchant_id, NEW.product_id, NEW.key, 1)
    ON CONFLICT (merchant_id, product_id, key) DO UPDATE SET revision = k.revision + 1, updated_at = now();
    RETURN NULL;
END $$;
CREATE TRIGGER price_key_revision AFTER INSERT OR UPDATE ON billing.prices
FOR EACH ROW EXECUTE FUNCTION billing.step_price_key();

CREATE FUNCTION billing.step_price_key_binding() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE binding billing.price_psp_bindings%ROWTYPE;
BEGIN
    IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN
        RETURN NULL;
    END IF;
    IF TG_OP='DELETE' THEN binding := OLD; ELSE binding := NEW; END IF;
    UPDATE billing.price_keys k SET revision = k.revision + 1, updated_at = now()
      FROM billing.prices p
     WHERE p.merchant_id = binding.merchant_id AND p.id = binding.price_id
       AND k.merchant_id = p.merchant_id AND k.product_id = p.product_id AND k.key = p.key;
    RETURN NULL;
END $$;
CREATE TRIGGER price_key_binding_revision AFTER INSERT OR UPDATE OR DELETE ON billing.price_psp_bindings
FOR EACH ROW EXECUTE FUNCTION billing.step_price_key_binding();

-- Field owners and price keys are merchant rows and occupy a restore destination.
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
                  'catalog_field_owners',
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
                  'destructive_run_before_images',
                  'document_sequences',
                  'failed_usage_windows',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'invoice_collection_cadence',
                  'invoice_items',
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
                  'order_lines',
                  'orders',
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
