-- Cards a vault must be asked about are refreshed one at a time
-- (account-updater real-time), queued on a decline or ahead of a renewal. The
-- batch account updater, its job table and its intents are gone.
DELETE FROM billing.provider_intents WHERE intent_type = 'bt_account_updater_batch';
DROP TABLE billing.account_updater_batches;

DROP INDEX billing.payment_methods_custodian_account_updater_checked_at_idx;
COMMENT ON COLUMN billing.payment_methods.account_updater_checked_at IS 'When the card''s vault last answered an account-updater request for it; NULL when never.';

-- Due work for the ahead-of-renewal pass: one row per vault card whose
-- renewals may need a check. A subscription period, status or card change
-- (and a default card change) marks its card -infinity; the pass plans it to
-- its next check or deletes the row.
CREATE TABLE billing.stored_card_checks (
    merchant_id uuid NOT NULL,
    payment_method_id uuid NOT NULL,
    due_at timestamp with time zone NOT NULL,
    CONSTRAINT stored_card_checks_pkey PRIMARY KEY (merchant_id, payment_method_id),
    CONSTRAINT stored_card_checks_payment_method_id_fkey FOREIGN KEY (merchant_id, payment_method_id) REFERENCES billing.payment_methods(merchant_id, id) ON DELETE CASCADE
);
CREATE INDEX stored_card_checks_due_at_idx ON billing.stored_card_checks USING btree (due_at);
COMMENT ON TABLE billing.stored_card_checks IS 'When the ahead-of-renewal pass next looks at a card a vault must be asked about; -infinity after its renewals changed. Derived from subscriptions; deleted when nothing is due.';

CREATE FUNCTION billing.mark_stored_card_check(p_merchant uuid, p_method uuid) RETURNS void
    LANGUAGE sql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
INSERT INTO billing.stored_card_checks (merchant_id, payment_method_id, due_at)
SELECT pm.merchant_id, pm.id, '-infinity' FROM billing.payment_methods pm
 WHERE pm.merchant_id = p_merchant AND pm.id = p_method
   AND pm.custodian IN ('basis_theory', 'platform') AND pm.status = 'active'
ON CONFLICT (merchant_id, payment_method_id) DO UPDATE SET due_at = '-infinity';
$$;

CREATE FUNCTION billing.subscription_marks_stored_card_check() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.deleted_at IS NOT NULL OR NEW.current_period_ends_at IS NULL OR NEW.status NOT IN ('active', 'past_due', 'awaiting_method') THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.current_period_ends_at IS NOT DISTINCT FROM NEW.current_period_ends_at AND OLD.status IS NOT DISTINCT FROM NEW.status
       AND OLD.payment_method_id IS NOT DISTINCT FROM NEW.payment_method_id AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at THEN
        RETURN NULL;
    END IF;
    PERFORM billing.mark_stored_card_check(NEW.merchant_id,
        billing.subscription_payment_method_id(NEW.merchant_id, NEW.customer_id, NEW.payment_method_id, NEW.price_id, NEW.rail, NEW.collection_policy));
    RETURN NULL;
END;
$$;
CREATE TRIGGER subscriptions_mark_stored_card_check AFTER INSERT OR UPDATE ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscription_marks_stored_card_check();

CREATE FUNCTION billing.default_card_marks_stored_card_check() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.default_payment_method_id IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.default_payment_method_id IS DISTINCT FROM NEW.default_payment_method_id) THEN
        PERFORM billing.mark_stored_card_check(NEW.merchant_id, NEW.default_payment_method_id);
    END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER money_settings_mark_stored_card_check AFTER INSERT OR UPDATE OF default_payment_method_id ON billing.money_settings
    FOR EACH ROW EXECUTE FUNCTION billing.default_card_marks_stored_card_check();

INSERT INTO billing.stored_card_checks (merchant_id, payment_method_id, due_at)
SELECT merchant_id, id, '-infinity' FROM billing.payment_methods
 WHERE custodian IN ('basis_theory', 'platform') AND status = 'active';

DROP INDEX billing.provider_intents_updated_at_idx;
CREATE INDEX provider_intents_updated_at_idx ON billing.provider_intents USING btree (merchant_id, updated_at)
    WHERE status IN ('succeeded', 'failed_terminal', 'superseded', 'expired')
      AND destructive_run_id IS NULL
      AND intent_type IN ('nmi_delete_subscription', 'stripe_cancel_subscription', 'ccbill_cancel_subscription', 'nmi_payment_method_update', 'nmi_payment_source_update', 'nmi_card_vault', 'network_token', 'stripe_archive_price', 'stripe_archive_product', 'solana_sunset_plan', 'account_updater');

DELETE FROM billing.worker_state WHERE worker_kind IN ('openrails.account_updater_batch', 'openrails.card_refresh');

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
                  'stored_card_checks',
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
