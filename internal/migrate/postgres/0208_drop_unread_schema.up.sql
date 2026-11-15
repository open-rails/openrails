-- Repair: none-needed. Removes what nothing reads; no finding names a reconciliation run once its foreign keys go.
-- The retired merchant alert inbox and the usage collection cadence.
DROP TABLE billing.notifications;
DROP TABLE billing.usage_collection_cadence;

-- A reconciliation pass is no maintenance run: its id only correlates the
-- findings it saw.
ALTER TABLE billing.reconciliation_findings
    DROP CONSTRAINT reconciliation_findings_first_seen_run_seen_run_class_fkey,
    DROP CONSTRAINT reconciliation_findings_last_seen_run_seen_run_class_fkey,
    DROP COLUMN seen_run_class;
DROP INDEX billing.reconciliation_findings_first_seen_run_idx;
DROP INDEX billing.reconciliation_findings_last_seen_run_idx;
COMMENT ON COLUMN billing.reconciliation_findings.first_seen_run IS 'The reconciliation pass that first saw this finding, an id naming no row; NULL when raised outside a pass (e.g. the intents volume breaker).';
COMMENT ON COLUMN billing.reconciliation_findings.last_seen_run IS 'The reconciliation pass that last saw this finding; a pass resolves the findings it did not see again.';

DROP TRIGGER retained_reconciliation_runs ON billing.maintenance_runs;
DELETE FROM billing.maintenance_runs WHERE kind = 'reconciliation';
DROP TRIGGER immutable_maintenance_runs_delete ON billing.maintenance_runs;
CREATE TRIGGER immutable_maintenance_runs_delete BEFORE DELETE ON billing.maintenance_runs
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- The pass's columns go with it, and dry_run, which no writer set.
DROP TRIGGER immutable_maintenance_run_facts ON billing.maintenance_runs;
ALTER TABLE billing.maintenance_runs
    DROP CONSTRAINT maintenance_runs_kind_check,
    DROP COLUMN mode,
    DROP COLUMN rails,
    DROP COLUMN window_starts_at,
    DROP COLUMN window_ends_at,
    DROP COLUMN dry_run,
    DROP COLUMN error;
ALTER TABLE billing.maintenance_runs ADD CONSTRAINT maintenance_runs_kind_check CHECK ((
    (kind IN ('prune','approval','merchant_purge') AND btrim(actor) <> ''
     AND summary IS NULL AND inventory_manifest IS NULL AND inventory_total_rows IS NULL)
    OR (kind = 'billing_restore' AND actor = 'merchantarchive' AND status IN ('running','completed')
     AND psp_id IS NULL AND coverage IS NULL AND expected_rows IS NULL AND affected IS NULL
     AND reversed_at IS NULL AND reversed_by IS NULL AND note IS NULL
     AND inventory_manifest IS NULL AND inventory_total_rows IS NULL
     AND ((status='running' AND finished_at IS NULL AND summary IS NULL)
          OR (status='completed' AND finished_at IS NOT NULL AND jsonb_typeof(summary)='object'
              AND summary ?& ARRAY['digest','rows'] AND jsonb_typeof(summary->'digest')='string'
              AND jsonb_typeof(summary->'rows')='number' AND summary->>'digest' ~ '^[0-9a-f]{64}$' AND (summary->>'rows')::bigint >= 0)))
    OR (kind = 'purge_inventory' AND status = 'completed' AND finished_at IS NOT NULL AND psp_id IS NULL
     AND coverage IS NULL AND expected_rows IS NULL AND affected IS NULL
     AND reversed_at IS NULL AND reversed_by IS NULL AND summary IS NULL
     AND inventory_manifest IS NOT NULL AND jsonb_typeof(inventory_manifest) = 'object'
     AND jsonb_typeof(inventory_manifest->'total_rows') = 'number'
     AND inventory_total_rows IS NOT NULL AND inventory_total_rows >= 0
     AND inventory_total_rows = (inventory_manifest->>'total_rows')::bigint)
) IS TRUE);
ALTER TABLE billing.maintenance_runs ALTER COLUMN run_class SET EXPRESSION AS
    (CASE WHEN kind = 'purge_inventory' THEN 'inventory' WHEN kind = 'billing_restore' THEN 'restore' ELSE 'destructive' END);
CREATE TRIGGER immutable_maintenance_run_facts BEFORE UPDATE ON billing.maintenance_runs
    FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('finished_at','status','summary','affected','reversed_at','reversed_by','note','run_class');
COMMENT ON TABLE billing.maintenance_runs IS 'Typed maintenance run headers: reversible destructive work, billing restore receipts and immutable purge inventories. Each kind has explicit columns and constraints; before-images remain in destructive_run_before_images. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.maintenance_runs.run_class IS 'Derived from the immutable kind. Child foreign keys include it, so stamped rows, intents and before-images reference only destructive runs.';

-- Never written.
ALTER TABLE billing.order_attempts DROP COLUMN routing_reason;
ALTER TABLE billing.payment_methods DROP COLUMN wallet_card_id;

-- Indexes another index covers: the primary key; the unique key whose prefix
-- this was; orders_id_idx; the unique index, whose event clause the CHECKs
-- already imply. The function's trigger went with the old event_log table.
DROP INDEX billing.destructive_holds_merchant_idx;
DROP INDEX billing.prices_key_idx;
DROP INDEX billing.orders_embedded_idx;
DROP INDEX billing.grants_supersedes_id_idx;
DROP INDEX billing.grants_supersedes_id_key;
CREATE UNIQUE INDEX grants_supersedes_id_key ON billing.grants USING btree (merchant_id, supersedes_id) WHERE (supersedes_id IS NOT NULL);
DROP FUNCTION billing.guard_event_log_fact();

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
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customer_low_balance_lines',
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
                  'stored_card_checks',
                  'subscription_status_transitions',
                  'subscription_billing_changes',
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
    END IF;
    RETURN NEW;
END;
$$;
