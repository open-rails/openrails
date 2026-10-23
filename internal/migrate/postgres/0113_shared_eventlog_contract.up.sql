-- Repair: none-needed. Preserve every retained event position, ID and cursor boundary.
-- Stop writers for this contract migration. Source capture stays transactional.
LOCK TABLE billing.merchants,billing.host_outbox,billing.notification_events,
 billing.event_log_streams,billing.event_log,billing.event_log_pending IN ACCESS EXCLUSIVE MODE;

-- BEGIN helpers/eventlog/postgres schema v1 (verbatim; checked by test)
CREATE TABLE IF NOT EXISTS "billing".eventlog_migrations(version integer PRIMARY KEY);
CREATE TABLE "billing".eventlog_streams (
 source text NOT NULL, scope text NOT NULL,
 epoch text NOT NULL DEFAULT gen_random_uuid()::text,
 position bigint NOT NULL DEFAULT 0,
 head_id text NOT NULL DEFAULT '',
 pruned_through bigint NOT NULL DEFAULT 0,
 pruned_id text NOT NULL DEFAULT '',
 recorded_at timestamptz NOT NULL DEFAULT '-infinity',
 PRIMARY KEY(source,scope)
);
CREATE TABLE "billing".eventlog_events (
 source text NOT NULL, scope text NOT NULL, position bigint NOT NULL,
 id text NOT NULL, event_source text NOT NULL, event_type text NOT NULL, version integer NOT NULL,
 occurred_at timestamptz NOT NULL, recorded_at timestamptz NOT NULL,
 subject_type text NOT NULL, subject_id text NOT NULL, data jsonb NOT NULL,
 PRIMARY KEY(source,scope,position), UNIQUE(source,scope,id),
 FOREIGN KEY(source,scope) REFERENCES "billing".eventlog_streams(source,scope) ON DELETE CASCADE
);
CREATE INDEX ON "billing".eventlog_events(source,scope,event_type,position);
CREATE INDEX ON "billing".eventlog_events(source,scope,recorded_at,position);
-- A queue row is part of the mutation transaction and is gone before commit.
-- The identity orders facts inside that transaction, never public cursors.
CREATE TABLE "billing".eventlog_pending (
 ordinal bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 transaction_id xid8 NOT NULL DEFAULT pg_current_xact_id(),
 source text NOT NULL CHECK(source<>''), scope text NOT NULL CHECK(scope<>''),
 id text NOT NULL CHECK(id<>''), event_source text NOT NULL, event_type text NOT NULL CHECK(event_type<>''),
 version integer NOT NULL CHECK(version>0), occurred_at timestamptz NOT NULL,
 subject_type text NOT NULL DEFAULT '', subject_id text NOT NULL DEFAULT '', data jsonb NOT NULL
);
CREATE INDEX ON "billing".eventlog_pending(transaction_id);
CREATE FUNCTION "billing".eventlog_flush() RETURNS trigger LANGUAGE plpgsql SET search_path TO pg_catalog,pg_temp AS $fn$
DECLARE
 s record; e record; next_position bigint; stamp timestamptz;
BEGIN
 -- Every scope touched by this transaction is known before taking any locks.
 FOR s IN SELECT DISTINCT source,scope FROM "billing".eventlog_pending
          WHERE transaction_id=pg_current_xact_id() ORDER BY source,scope
 LOOP
  INSERT INTO "billing".eventlog_streams(source,scope) VALUES(s.source,s.scope) ON CONFLICT DO NOTHING;
  PERFORM 1 FROM "billing".eventlog_streams WHERE source=s.source AND scope=s.scope FOR UPDATE;
 END LOOP;
 FOR e IN SELECT * FROM "billing".eventlog_pending WHERE transaction_id=pg_current_xact_id() ORDER BY ordinal
 LOOP
  IF EXISTS(SELECT 1 FROM "billing".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id) THEN
   IF EXISTS(SELECT 1 FROM "billing".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id
     AND event_source=e.event_source AND event_type=e.event_type AND version=e.version
     AND occurred_at=e.occurred_at AND subject_type=e.subject_type AND subject_id=e.subject_id AND data=e.data) THEN
    CONTINUE;
   END IF;
   RAISE EXCEPTION 'event ID already records a different fact' USING ERRCODE='23505';
  END IF;
  UPDATE "billing".eventlog_streams SET position=position+1,head_id=e.id,
   recorded_at=greatest(recorded_at,clock_timestamp())
   WHERE source=e.source AND scope=e.scope RETURNING position,recorded_at INTO next_position,stamp;
  INSERT INTO "billing".eventlog_events(source,scope,position,id,event_source,event_type,version,occurred_at,recorded_at,subject_type,subject_id,data)
   VALUES(e.source,e.scope,next_position,e.id,e.event_source,e.event_type,e.version,e.occurred_at,stamp,e.subject_type,e.subject_id,e.data);
 END LOOP;
 DELETE FROM "billing".eventlog_pending WHERE transaction_id=pg_current_xact_id();
 RETURN NULL;
END $fn$;
CREATE CONSTRAINT TRIGGER eventlog_flush_at_commit AFTER INSERT ON "billing".eventlog_pending
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION "billing".eventlog_flush();

CREATE FUNCTION "billing".eventlog_immutable() RETURNS trigger LANGUAGE plpgsql SET search_path TO pg_catalog,pg_temp AS $fn$
BEGIN
 RAISE EXCEPTION 'retained event facts are immutable';
END $fn$;
CREATE TRIGGER eventlog_immutable BEFORE UPDATE ON "billing".eventlog_events
 FOR EACH ROW EXECUTE FUNCTION "billing".eventlog_immutable();

INSERT INTO "billing".eventlog_migrations(version) VALUES(1);
-- END helpers/eventlog/postgres schema v1

-- Product ownership is independent of the shared log implementation. These
-- generated keys preserve merchant deletion cascades and archive inspection.
ALTER TABLE billing.eventlog_streams ADD COLUMN merchant_id uuid
 GENERATED ALWAYS AS (CASE WHEN source='openrails' THEN scope::uuid END) STORED
 REFERENCES billing.merchants(id) ON DELETE CASCADE;
ALTER TABLE billing.eventlog_events ADD COLUMN merchant_id uuid
 GENERATED ALWAYS AS (CASE WHEN source='openrails' THEN scope::uuid END) STORED;
CREATE UNIQUE INDEX eventlog_merchant_stream ON billing.eventlog_streams(merchant_id) WHERE source='openrails';
CREATE INDEX eventlog_merchant_position ON billing.eventlog_events(merchant_id,position);
CREATE INDEX eventlog_merchant_type_position ON billing.eventlog_events(merchant_id,event_type,position);
CREATE INDEX eventlog_merchant_retention ON billing.eventlog_events(merchant_id,recorded_at,position);
COMMENT ON TABLE billing.eventlog_events IS 'Shared retained merchant facts. Retention: events expire after the configured event retention, independently of reader progress.';
COMMENT ON TABLE billing.eventlog_streams IS 'Shared committed event positions and retention boundaries; merchant-owned state.';
COMMENT ON TABLE billing.eventlog_pending IS 'Shared transaction scratch; empty after commit.';
COMMENT ON TABLE billing.eventlog_migrations IS 'Installed shared event-log schema versions.';

INSERT INTO billing.eventlog_streams(source,scope,epoch,position,head_id,pruned_through,pruned_id,recorded_at)
 SELECT 'openrails',merchant_id::text,epoch::text,position,head_id,pruned_through,pruned_id,recorded_at FROM billing.event_log_streams;
INSERT INTO billing.eventlog_events(source,scope,position,id,event_source,event_type,version,occurred_at,recorded_at,subject_type,subject_id,data)
 SELECT 'openrails',merchant_id::text,position,id,source,event_type,version,occurred_at,recorded_at,subject_type,subject_id,data FROM billing.event_log;

CREATE OR REPLACE FUNCTION billing.create_event_log_stream() RETURNS trigger
LANGUAGE plpgsql SET search_path TO pg_catalog,billing,pg_temp AS $$
BEGIN
 INSERT INTO billing.eventlog_streams(source,scope) VALUES('openrails',NEW.id::text) ON CONFLICT DO NOTHING;
 RETURN NEW;
END;
$$;
CREATE OR REPLACE FUNCTION billing.queue_event_log(event jsonb) RETURNS void
LANGUAGE plpgsql SET search_path TO pg_catalog,billing,pg_temp AS $$
DECLARE pinned text:=NULLIF(current_setting('openrails.merchant_id',true),'');
BEGIN
 IF pinned IS NOT NULL AND pinned IS DISTINCT FROM event->>'merchant_id' THEN
  RAISE EXCEPTION 'event source differs from the pinned merchant' USING ERRCODE='42501';
 END IF;
 INSERT INTO billing.eventlog_pending(source,scope,id,event_source,event_type,version,occurred_at,subject_type,subject_id,data)
 VALUES('openrails',event->>'merchant_id',event->>'id',event->>'source',event->>'type',
  coalesce((event->>'version')::integer,1),(event->>'occurred_at')::timestamptz,
  event->>'subject_type',event->>'subject_id',event->'data');
END;
$$;
-- A scope deleted in the same transaction must not be recreated by deferred
-- capture. Concurrent source writes already share merchant FK serialization.
CREATE FUNCTION billing.discard_deleted_merchant_eventlog() RETURNS trigger
LANGUAGE plpgsql SET search_path TO pg_catalog,billing,pg_temp AS $$
BEGIN
 DELETE FROM billing.eventlog_pending WHERE source='openrails' AND scope=OLD.id::text;
 RETURN OLD;
END;
$$;
CREATE TRIGGER discard_deleted_merchant_eventlog BEFORE DELETE ON billing.merchants
 FOR EACH ROW EXECUTE FUNCTION billing.discard_deleted_merchant_eventlog();

DROP TABLE billing.event_log_pending;
DROP FUNCTION billing.flush_event_log();
DROP TABLE billing.event_log;
DROP TABLE billing.event_log_streams;

-- Read/prune SQL from the previous reader release remains compatible. Domain
-- source triggers above now use the shared queue and its shared deferred flush.
CREATE VIEW billing.event_log AS
 SELECT merchant_id,position,id,event_source AS source,event_type,version,
 occurred_at,recorded_at,subject_type,subject_id,data
 FROM billing.eventlog_events WHERE source='openrails';
CREATE VIEW billing.event_log_streams AS
 SELECT merchant_id,epoch::uuid AS epoch,position,head_id,pruned_through,pruned_id,recorded_at
 FROM billing.eventlog_streams WHERE source='openrails';
CREATE FUNCTION billing.update_legacy_eventlog_stream() RETURNS trigger
LANGUAGE plpgsql SET search_path TO pg_catalog,billing,pg_temp AS $$
BEGIN
 IF NEW.merchant_id IS DISTINCT FROM OLD.merchant_id THEN
  RAISE EXCEPTION 'event scope identity is immutable' USING ERRCODE='23514';
 END IF;
 UPDATE billing.eventlog_streams SET epoch=NEW.epoch::text,position=NEW.position,
 head_id=NEW.head_id,pruned_through=NEW.pruned_through,pruned_id=NEW.pruned_id,
 recorded_at=NEW.recorded_at WHERE source='openrails' AND scope=OLD.merchant_id::text;
 RETURN NEW;
END;
$$;
CREATE TRIGGER update_legacy_eventlog_stream INSTEAD OF UPDATE ON billing.event_log_streams
 FOR EACH ROW EXECUTE FUNCTION billing.update_legacy_eventlog_stream();

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
                  'eventlog_events',
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
