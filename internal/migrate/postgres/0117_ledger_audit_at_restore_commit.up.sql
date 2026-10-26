-- The deferred restore constraint validates the final ledger before COMMIT.
-- Finalizing its receipt needs no earlier full-ledger scans. Keep receipt
-- identity/state checks and the deferred constraint unchanged.

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
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION billing.finish_billing_restore(p_merchant uuid,p_digest text,p_rows bigint) RETURNS void
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NOT billing.billing_restore_active(p_merchant) OR p_digest IS NULL OR p_rows IS NULL OR p_digest !~ '^[0-9a-f]{64}$' OR p_rows<0 THEN
        RAISE EXCEPTION 'invalid billing restore finalization' USING ERRCODE='23514';
    END IF;
    UPDATE billing.maintenance_runs SET status='completed',finished_at=now(),summary=jsonb_build_object('digest',p_digest,'rows',p_rows)
        WHERE merchant_id=p_merchant AND kind='billing_restore' AND id::text=current_setting('openrails.billing_restore_id',true);
END;
$$;
