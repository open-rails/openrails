-- Repair: none-needed. Retained facts are independent of legacy delivery ACKs.
-- A single deferred flush locks all merchants touched by the transaction in
-- deterministic order; counter positions therefore follow committed order.
LOCK TABLE billing.merchants,billing.host_outbox,billing.notification_events IN SHARE ROW EXCLUSIVE MODE;
CREATE TABLE billing.event_log_streams (
    merchant_id uuid PRIMARY KEY REFERENCES billing.merchants(id) ON DELETE CASCADE,
    epoch uuid NOT NULL DEFAULT uuidv7(),
    position bigint NOT NULL DEFAULT 0 CHECK (position >= 0),
    head_id text NOT NULL DEFAULT '',
    pruned_through bigint NOT NULL DEFAULT 0 CHECK (pruned_through >= 0),
    pruned_id text NOT NULL DEFAULT '',
    recorded_at timestamptz NOT NULL DEFAULT '-infinity'
);
CREATE TABLE billing.event_log (
    merchant_id uuid NOT NULL REFERENCES billing.event_log_streams(merchant_id) ON DELETE CASCADE,
    position bigint NOT NULL CHECK (position > 0),
    id text NOT NULL,
    source text NOT NULL CHECK (source IN ('host','notification')),
    event_type text NOT NULL,
    version integer NOT NULL DEFAULT 1 CHECK (version > 0),
    occurred_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL,
    subject_type text NOT NULL,
    subject_id text NOT NULL,
    data jsonb NOT NULL,
    PRIMARY KEY(merchant_id, position),
    UNIQUE(merchant_id,id)
);
CREATE INDEX event_log_type_position_idx ON billing.event_log(merchant_id,event_type,position);
CREATE INDEX event_log_retention_idx ON billing.event_log(recorded_at,merchant_id,position);
CREATE INDEX event_log_merchant_retention_idx ON billing.event_log(merchant_id,recorded_at,position);
COMMENT ON TABLE billing.event_log IS 'Retained merchant facts, independent of readers. Retention: events expire after the configured event retention, independently of reader progress.';
COMMENT ON TABLE billing.event_log_streams IS 'Committed event positions and retention boundaries; no reader state. State owned by the merchant.';

-- This is private transaction scratch, not a queue: its row is inserted,
-- consumed and deleted in the same successful commit; rollback exposes none.
CREATE TABLE billing.event_log_pending (
    transaction_id bigint PRIMARY KEY,
    entries jsonb NOT NULL CHECK (jsonb_typeof(entries)='array')
);
COMMENT ON TABLE billing.event_log_pending IS 'Internal transaction scratch; no row survives a successful commit. Exists only for the transaction lifetime.';

CREATE FUNCTION billing.event_subject_id(kind text, id uuid) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE SET search_path TO 'pg_catalog','billing','pg_temp' AS $$
    SELECT CASE WHEN id IS NULL THEN NULL ELSE
      CASE kind WHEN 'payment' THEN 'pay_' WHEN 'product' THEN 'prod_'
      WHEN 'subscription' THEN 'sub_' WHEN 'order' THEN 'ord_'
      WHEN 'mandate' THEN 'mdt_' WHEN 'destructive_hold' THEN 'dsh_' ELSE '' END || id::text END
$$;

CREATE FUNCTION billing.event_log_data(value jsonb) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE SET search_path TO 'pg_catalog','billing','pg_temp' AS $$
DECLARE key text; prefix text;
BEGIN
    value := COALESCE(value,'{}'::jsonb);
    -- Keep monetary integers lossless for JavaScript consumers, just like the
    -- existing typed billing API. Do not reinterpret arbitrary payload fields.
    FOREACH key IN ARRAY ARRAY['amount','total','overdue_amount','amount_floor',
      'available_amount','owed_amount','accrual_rate_per_hour','usage_rate_per_hour',
      'low_balance_amount','old_amount','new_amount','amount_due'] LOOP
        IF jsonb_typeof(value->key)='number' THEN
            value:=jsonb_set(value,ARRAY[key],to_jsonb(value->>key));
        END IF;
    END LOOP;
    FOR key,prefix IN SELECT * FROM (VALUES
      ('payment_id','pay_'),('product_id','prod_'),('subscription_id','sub_'),
      ('order_id','ord_'),('mandate_id','mdt_'),('price_migration_id','pmig_'),
      ('from_product_id','prod_'),('to_product_id','prod_')) AS ids(key,prefix) LOOP
        IF value->>key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
            value:=jsonb_set(value,ARRAY[key],to_jsonb(prefix||(value->>key)));
        END IF;
    END LOOP;
    RETURN value;
END;
$$;

CREATE FUNCTION billing.host_event_record(h billing.host_outbox) RETURNS jsonb
LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog','billing','pg_temp' AS $$
DECLARE payload jsonb; payment record;
    source_merchant uuid:=h.merchant_id; source_payment uuid:=h.payment_id;
    account uuid; authority text;
BEGIN
    payload:=h.data || jsonb_strip_nulls(jsonb_build_object('currency',NULLIF(h.currency,''),'amount',h.amount::text));
    IF h.subject_type='customer' THEN payload:=payload||jsonb_build_object('customer_id',h.subject_id::text); END IF;
    IF h.subject_type='order' THEN payload:=payload||jsonb_build_object('order_id','ord_'||h.subject_id); END IF;
    IF h.subject_type='product' THEN payload:=payload||jsonb_build_object('product_id','prod_'||h.subject_id); END IF;
    IF h.subject_type='mandate' THEN payload:=payload||jsonb_build_object('mandate_id','mdt_'||h.subject_id); END IF;
    IF h.payment_id IS NOT NULL THEN
        SELECT customer_id,subscription_id,order_id INTO payment
        FROM billing.payments WHERE merchant_id=source_merchant AND id=source_payment;
        payload:=payload||jsonb_strip_nulls(jsonb_build_object('payment_id','pay_'||h.payment_id,
          'customer_id',payment.customer_id,'subscription_id',payment.subscription_id,'order_id',payment.order_id));
    END IF;
    IF payload ? 'customer_id' THEN
        account:=(payload->>'customer_id')::uuid;
        SELECT COALESCE(issuer,'') INTO authority FROM billing.customers
          WHERE merchant_id=source_merchant AND id=account;
        payload:=payload||jsonb_build_object('account_id',account,'account_issuer',authority);
    END IF;
    RETURN jsonb_build_object('merchant_id',h.merchant_id,'id','hev_'||h.id,'source','host',
      'type',h.event_type,'occurred_at',h.occurred_at,'subject_type',h.subject_type,
      'subject_id',billing.event_subject_id(h.subject_type,h.subject_id),'data',billing.event_log_data(payload));
END;
$$;

CREATE FUNCTION billing.notification_event_record(n billing.notification_events) RETURNS jsonb
LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog','billing','pg_temp' AS $$
DECLARE source_merchant uuid:=n.merchant_id; account uuid:=n.customer_id; authority text;
BEGIN
    SELECT COALESCE(issuer,'') INTO authority FROM billing.customers
      WHERE merchant_id=source_merchant AND id=account;
    RETURN jsonb_build_object('merchant_id',n.merchant_id,'id','ntf_'||n.id,'source','notification',
      'type',n.event_type,'occurred_at',n.created_at,'subject_type','customer',
      'subject_id',n.customer_id::text,'data',billing.event_log_data(n.data||
        jsonb_build_object('customer_id',account,'account_id',account,'account_issuer',authority)));
END;
$$;

INSERT INTO billing.event_log_streams(merchant_id) SELECT id FROM billing.merchants ORDER BY id;
-- All preexisting rows are already committed. Preserve their stable IDs and
-- order them deterministically; their new retention starts at this migration.
WITH history AS (
    SELECT billing.host_event_record(h) AS event FROM billing.host_outbox h
    UNION ALL SELECT billing.notification_event_record(n) FROM billing.notification_events n
), ordered AS (
    SELECT event, row_number() OVER (PARTITION BY event->>'merchant_id'
       ORDER BY (event->>'occurred_at')::timestamptz,event->>'source',event->>'id') AS position FROM history
)
INSERT INTO billing.event_log(merchant_id,position,id,source,event_type,occurred_at,recorded_at,subject_type,subject_id,data)
SELECT (event->>'merchant_id')::uuid,position,event->>'id',event->>'source',event->>'type',
       (event->>'occurred_at')::timestamptz,statement_timestamp(),event->>'subject_type',event->>'subject_id',event->'data'
FROM ordered;
UPDATE billing.event_log_streams s SET position=e.position,head_id=e.id,recorded_at=e.recorded_at
FROM (SELECT DISTINCT ON (merchant_id) merchant_id,position,id,recorded_at FROM billing.event_log ORDER BY merchant_id,position DESC) e
WHERE s.merchant_id=e.merchant_id;

CREATE FUNCTION billing.create_event_log_stream() RETURNS trigger LANGUAGE plpgsql SET search_path TO 'pg_catalog','billing','pg_temp' AS $$
BEGIN
    INSERT INTO billing.event_log_streams(merchant_id) VALUES(NEW.id) ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$;
CREATE TRIGGER merchant_event_log_stream AFTER INSERT ON billing.merchants
FOR EACH ROW EXECUTE FUNCTION billing.create_event_log_stream();

CREATE FUNCTION billing.queue_event_log(event jsonb) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog','billing','pg_temp' AS $$
DECLARE scope text:=NULLIF(current_setting('openrails.merchant_id',true),'');
BEGIN
    IF scope IS NOT NULL AND scope IS DISTINCT FROM event->>'merchant_id' THEN
        RAISE EXCEPTION 'event source differs from the pinned merchant' USING ERRCODE='42501';
    END IF;
    INSERT INTO billing.event_log_pending(transaction_id,entries) VALUES(txid_current(),jsonb_build_array(event))
    ON CONFLICT(transaction_id) DO UPDATE SET entries=billing.event_log_pending.entries||EXCLUDED.entries;
END;
$$;
CREATE FUNCTION billing.capture_host_event_log() RETURNS trigger LANGUAGE plpgsql SET search_path TO 'pg_catalog','billing','pg_temp' AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    PERFORM billing.queue_event_log(billing.host_event_record(NEW));
    RETURN NEW;
END;
$$;
CREATE FUNCTION billing.capture_notification_event_log() RETURNS trigger LANGUAGE plpgsql SET search_path TO 'pg_catalog','billing','pg_temp' AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    PERFORM billing.queue_event_log(billing.notification_event_record(NEW));
    RETURN NEW;
END;
$$;
CREATE TRIGGER host_event_log_capture AFTER INSERT ON billing.host_outbox
FOR EACH ROW EXECUTE FUNCTION billing.capture_host_event_log();
CREATE TRIGGER notification_event_log_capture AFTER INSERT ON billing.notification_events
FOR EACH ROW EXECUTE FUNCTION billing.capture_notification_event_log();

CREATE FUNCTION billing.flush_event_log() RETURNS trigger LANGUAGE plpgsql SET search_path TO 'pg_catalog','billing','pg_temp' AS $$
DECLARE facts jsonb; merchant uuid; event jsonb; next_position bigint; committed_at timestamptz;
BEGIN
    -- Read the final row: NEW from its initial INSERT predates later events in
    -- this same transaction. An already-consumed deferred callback is a no-op.
    SELECT entries INTO facts FROM billing.event_log_pending WHERE transaction_id=NEW.transaction_id FOR UPDATE;
    IF NOT FOUND THEN RETURN NULL; END IF;
    -- Every transaction takes all its stream locks in the same order, across
    -- both source tables and every statement, before allocating any position.
    FOR merchant IN SELECT DISTINCT (value->>'merchant_id')::uuid FROM jsonb_array_elements(facts) ORDER BY 1 LOOP
        PERFORM 1 FROM billing.event_log_streams WHERE merchant_id=merchant FOR UPDATE;
    END LOOP;
    FOR event IN SELECT value FROM jsonb_array_elements(facts) LOOP
        merchant:=(event->>'merchant_id')::uuid;
        IF NOT EXISTS(SELECT 1 FROM billing.event_log_streams WHERE merchant_id=merchant) THEN CONTINUE; END IF;
        IF EXISTS(SELECT 1 FROM billing.event_log WHERE merchant_id=merchant AND id=event->>'id') THEN CONTINUE; END IF;
        UPDATE billing.event_log_streams SET position=position+1,
           recorded_at=greatest(recorded_at,clock_timestamp()),head_id=event->>'id'
        WHERE merchant_id=merchant RETURNING position,recorded_at INTO next_position,committed_at;
        INSERT INTO billing.event_log(merchant_id,position,id,source,event_type,occurred_at,recorded_at,subject_type,subject_id,data)
        VALUES(merchant,next_position,event->>'id',event->>'source',event->>'type',
           (event->>'occurred_at')::timestamptz,committed_at,event->>'subject_type',event->>'subject_id',event->'data');
    END LOOP;
    DELETE FROM billing.event_log_pending WHERE transaction_id=NEW.transaction_id;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER event_log_flush AFTER INSERT ON billing.event_log_pending
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION billing.flush_event_log();

CREATE FUNCTION billing.guard_event_log_fact() RETURNS trigger LANGUAGE plpgsql SET search_path TO 'pg_catalog','billing','pg_temp' AS $$
BEGIN
    IF to_jsonb(OLD) IS DISTINCT FROM to_jsonb(NEW) THEN
        RAISE EXCEPTION 'event log facts are immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER event_log_fact_immutable BEFORE UPDATE ON billing.event_log
FOR EACH ROW EXECUTE FUNCTION billing.guard_event_log_fact();

-- A retained log belongs to this deployment. A destination with retained facts
-- is occupied; its empty stream identity alone does not prevent a restore.
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
                  'event_log',
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
