-- Repair: none-needed New audit evidence starts here; historical causes remain unknown.
CREATE TABLE billing.subscription_billing_changes (
    merchant_id uuid NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    subscription_id uuid NOT NULL,
    occurred_at timestamptz DEFAULT now() NOT NULL,
    kind text NOT NULL,
    decision text,
    from_payment_method_id uuid,
    to_payment_method_id uuid,
    from_next_retry_at timestamptz,
    to_next_retry_at timestamptz,
    from_last_retry_at timestamptz,
    to_last_retry_at timestamptz,
    from_retry_attempts integer,
    to_retry_attempts integer,
    from_transient_retries integer,
    to_transient_retries integer,
    from_grace_ends_at timestamptz,
    to_grace_ends_at timestamptz,
    method_version_id uuid,
    origin text,
    actor text,
    executor text,
    operation_id uuid,
    PRIMARY KEY (merchant_id, id),
    FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions(merchant_id,id) ON DELETE RESTRICT,
    CHECK (kind IN ('card_selection', 'default_card', 'card_version', 'retry_schedule'))
);
COMMENT ON TABLE billing.subscription_billing_changes IS 'Append-only observed subscription card and retry changes. References and before/after values are snapshots, not charging instructions. Retention: rows are deleted 25 months (761 days) after occurred_at, by the cleanup job only.';
CREATE INDEX subscription_billing_changes_history_idx ON billing.subscription_billing_changes (merchant_id,subscription_id,occurred_at DESC,id DESC);
CREATE INDEX subscription_billing_changes_retention_idx ON billing.subscription_billing_changes (merchant_id,occurred_at);
CREATE TRIGGER immutable_subscription_billing_changes BEFORE UPDATE ON billing.subscription_billing_changes
FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER retained_subscription_billing_changes BEFORE DELETE ON billing.subscription_billing_changes
FOR EACH ROW EXECUTE FUNCTION billing.guard_retention_delete('occurred_at','761 days');
ALTER TABLE billing.subscription_status_transitions
    ADD COLUMN origin text,
    ADD COLUMN actor text,
    ADD COLUMN executor text,
    ADD COLUMN operation_id uuid;
ALTER TABLE billing.provider_mutation_logs
    ADD COLUMN origin text,
    ADD COLUMN actor text,
    ADD COLUMN executor text,
    ADD COLUMN operation_id uuid,
    ADD COLUMN subscription_id uuid,
    ADD COLUMN reason_code text;
CREATE INDEX provider_mutation_logs_subscription_history_idx ON billing.provider_mutation_logs (merchant_id,subscription_id,created_at DESC,id DESC) WHERE subscription_id IS NOT NULL;

CREATE INDEX provider_mutation_logs_operation_history_idx ON billing.provider_mutation_logs (merchant_id,operation_id,created_at DESC,id DESC) WHERE phase IN ('parked','attempting','succeeded','failed');

-- One exact write owns the attribution. A later write in the transaction
-- cannot borrow it, even when a tier change updates one row and inserts another.
CREATE FUNCTION billing.subscription_history_attribution(mid uuid, entity uuid, action text, version bigint) RETURNS jsonb
LANGUAGE plpgsql AS $$
DECLARE a jsonb := COALESCE(NULLIF(current_setting('openrails.subscription_history',true),''),'{}')::jsonb;
BEGIN
    IF a->>'merchant_id'=mid::text AND a->>'action'=action AND (
        (a->>'entity_id'=entity::text AND (a->>'version')::bigint=version)
        OR (action='update' AND (a->'versions'->>entity::text)::bigint=version)
    ) THEN RETURN a; END IF;
    RETURN '{}'::jsonb;
END;
$$;

CREATE OR REPLACE FUNCTION billing.subscriptions_record_status_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE a jsonb;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    IF TG_OP='INSERT' THEN
        a := billing.subscription_history_attribution(NEW.merchant_id,NEW.id,'insert',0);
        INSERT INTO billing.subscription_status_transitions
            (merchant_id,subscription_id,to_status,cancel_type,decision,to_current_period_ends_at,origin,actor,executor,operation_id)
        VALUES (NEW.merchant_id,NEW.id,NEW.status,NEW.cancel_type,'created',NEW.current_period_ends_at,
            a->>'origin',a->>'actor',a->>'executor',(a->>'operation_id')::uuid);
    ELSIF OLD.status IS DISTINCT FROM NEW.status OR OLD.current_period_ends_at IS DISTINCT FROM NEW.current_period_ends_at THEN
        a := billing.subscription_history_attribution(NEW.merchant_id,NEW.id,'update',OLD.row_version);
        INSERT INTO billing.subscription_status_transitions
            (merchant_id,subscription_id,from_status,to_status,cancel_type,decision,from_current_period_ends_at,to_current_period_ends_at,origin,actor,executor,operation_id)
        VALUES (NEW.merchant_id,NEW.id,OLD.status,NEW.status,NEW.cancel_type,a->>'decision',OLD.current_period_ends_at,NEW.current_period_ends_at,
            a->>'origin',a->>'actor',a->>'executor',(a->>'operation_id')::uuid);
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION billing.subscriptions_record_billing_change() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE a jsonb;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    a := billing.subscription_history_attribution(NEW.merchant_id,NEW.id,'update',OLD.row_version);
    IF OLD.payment_method_id IS DISTINCT FROM NEW.payment_method_id THEN
        INSERT INTO billing.subscription_billing_changes
            (merchant_id,subscription_id,kind,decision,from_payment_method_id,to_payment_method_id,origin,actor,executor,operation_id)
        VALUES (NEW.merchant_id,NEW.id,'card_selection',a->>'decision',OLD.payment_method_id,NEW.payment_method_id,
            a->>'origin',a->>'actor',a->>'executor',(a->>'operation_id')::uuid);
    END IF;
    IF ROW(OLD.next_retry_at,OLD.last_retry_at,OLD.retry_attempts,OLD.transient_retries,OLD.grace_ends_at) IS DISTINCT FROM ROW(NEW.next_retry_at,NEW.last_retry_at,NEW.retry_attempts,NEW.transient_retries,NEW.grace_ends_at) THEN
        INSERT INTO billing.subscription_billing_changes
            (merchant_id,subscription_id,kind,decision,from_next_retry_at,to_next_retry_at,from_last_retry_at,to_last_retry_at,from_retry_attempts,to_retry_attempts,from_transient_retries,to_transient_retries,from_grace_ends_at,to_grace_ends_at,origin,actor,executor,operation_id)
        VALUES (NEW.merchant_id,NEW.id,'retry_schedule',a->>'decision',OLD.next_retry_at,NEW.next_retry_at,OLD.last_retry_at,NEW.last_retry_at,OLD.retry_attempts,NEW.retry_attempts,OLD.transient_retries,NEW.transient_retries,OLD.grace_ends_at,NEW.grace_ends_at,
            a->>'origin',a->>'actor',a->>'executor',(a->>'operation_id')::uuid);
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_subscriptions_billing_changes AFTER UPDATE OF payment_method_id,next_retry_at,last_retry_at,retry_attempts,transient_retries,grace_ends_at ON billing.subscriptions
FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_record_billing_change();

CREATE FUNCTION billing.subscription_history_default_card() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE a jsonb;
BEGIN
    a := billing.subscription_history_attribution(NEW.merchant_id,NEW.customer_id,'default:'||NEW.currency,0);
    PERFORM set_config('openrails.subscription_history','',true);
    IF billing.billing_restore_active(NEW.merchant_id) OR OLD.default_payment_method_id IS NOT DISTINCT FROM NEW.default_payment_method_id THEN RETURN NEW; END IF;
    INSERT INTO billing.subscription_billing_changes
        (merchant_id,subscription_id,kind,from_payment_method_id,to_payment_method_id,origin,actor,executor,operation_id)
    SELECT s.merchant_id,s.id,'default_card',OLD.default_payment_method_id,NEW.default_payment_method_id,
        a->>'origin',a->>'actor',a->>'executor',(a->>'operation_id')::uuid
    FROM billing.subscriptions s JOIN billing.prices p ON p.merchant_id=s.merchant_id AND p.id=s.price_id
    WHERE s.merchant_id=NEW.merchant_id AND s.customer_id=NEW.customer_id AND p.currency=NEW.currency
      AND s.payment_method_id IS NULL AND s.rail IN ('stripe','nmi') AND s.collection_policy IN ('engine','nmi_schedule')
      AND s.deleted_at IS NULL AND s.status IN ('active','past_due','awaiting_method','unverified');
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_subscription_history_default AFTER UPDATE OF default_payment_method_id ON billing.money_settings
FOR EACH ROW EXECUTE FUNCTION billing.subscription_history_default_card();

CREATE FUNCTION billing.subscription_history_card_version() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE a jsonb;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    a := billing.subscription_history_attribution(NEW.merchant_id,NEW.id,'card_version',0);
    INSERT INTO billing.subscription_billing_changes
        (merchant_id,subscription_id,kind,decision,from_payment_method_id,to_payment_method_id,method_version_id,origin,actor,executor,operation_id)
    SELECT s.merchant_id,s.id,'card_version',NEW.kind,NEW.payment_method_id,NEW.payment_method_id,NEW.id,
        COALESCE(a->>'origin',NEW.source),a->>'actor',COALESCE(a->>'executor',NEW.source),(a->>'operation_id')::uuid
    FROM billing.subscriptions s
    WHERE s.merchant_id=NEW.merchant_id AND s.customer_id=NEW.customer_id
      AND billing.subscription_payment_method_id(s.merchant_id,s.customer_id,s.payment_method_id,s.price_id,s.rail,s.collection_policy)=NEW.payment_method_id
      AND s.deleted_at IS NULL AND s.status IN ('active','past_due','awaiting_method','unverified');
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_subscription_history_card_version AFTER INSERT ON billing.payment_method_versions
FOR EACH ROW EXECUTE FUNCTION billing.subscription_history_card_version();

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
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
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
                  'subscription_status_transitions',
                  'subscription_billing_changes',
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
