-- Stripe charges, refunds and disputes whose processor fees the general ledger
-- has yet to read. The payment or dispute that records one queues it in its
-- own transaction; the billing pass reads its balance transactions from
-- Stripe and posts each fee once. A read that fails waits and is read again:
-- a fee never holds up the money movement it belongs to.
CREATE TABLE billing.stripe_fee_reads (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    ref text NOT NULL CHECK (ref <> ''),
    kind text NOT NULL CHECK (kind IN ('payment', 'refund', 'dispute')),
    due_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error text CHECK (last_error <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, psp_id, ref),
    CONSTRAINT stripe_fee_reads_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.stripe_fee_reads IS 'A Stripe charge, refund or dispute whose balance transactions the general ledger has yet to read for their fees: queued by the payment or dispute that records it, read by the billing pass''s stripe_fees scan, deleted once its fees post.';
COMMENT ON COLUMN billing.stripe_fee_reads.ref IS 'The Stripe object as the payment or dispute names it: a charge, payment intent or invoice for a payment, a refund, or a dispute.';
COMMENT ON COLUMN billing.stripe_fee_reads.due_at IS 'When it is read next; a failed read waits longer each time, a dispute that changes is read again at once.';

CREATE INDEX stripe_fee_reads_due_at_idx ON billing.stripe_fee_reads (due_at, merchant_id, psp_id, ref);

-- A Stripe payment that moved money, a Stripe refund, or a Stripe dispute
-- (again on each change) queues its fee read. A chargeback's own payment rows
-- read through their dispute.
CREATE FUNCTION billing.queue_stripe_fee_read() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NULL; END IF;
    IF TG_TABLE_NAME = 'payments' THEN
        IF NEW.rail IS DISTINCT FROM 'stripe' OR NEW.psp_id IS NULL OR NEW.money_movement <> 'rail' OR NEW.status <> 'succeeded'
           OR NEW.amount = 0 OR NEW.deleted_at IS NOT NULL OR NEW.transaction_id = ''
           OR coalesce(NEW.reversal_kind IN ('chargeback', 'dispute_reversal'), false)
           OR TG_OP = 'UPDATE' AND OLD.status = 'succeeded' AND OLD.money_movement = 'rail' THEN
            RETURN NULL;
        END IF;
        INSERT INTO billing.stripe_fee_reads (merchant_id, psp_id, ref, kind, due_at)
        VALUES (NEW.merchant_id, NEW.psp_id, NEW.transaction_id, CASE WHEN NEW.reversal_kind = 'refund' THEN 'refund' ELSE 'payment' END, now())
        ON CONFLICT DO NOTHING;
    ELSIF NEW.rail = 'stripe' AND NEW.provider_ref <> '' THEN
        INSERT INTO billing.stripe_fee_reads (merchant_id, psp_id, ref, kind, due_at)
        VALUES (NEW.merchant_id, NEW.psp_id, NEW.provider_ref, 'dispute', now())
        ON CONFLICT (merchant_id, psp_id, ref) DO UPDATE SET due_at = now(), attempts = 0, last_error = NULL;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER payments_queue_stripe_fee_read AFTER INSERT OR UPDATE OF status, money_movement ON billing.payments
    FOR EACH ROW EXECUTE FUNCTION billing.queue_stripe_fee_read();
CREATE TRIGGER disputes_queue_stripe_fee_read AFTER INSERT OR UPDATE OF status ON billing.disputes
    FOR EACH ROW EXECUTE FUNCTION billing.queue_stripe_fee_read();

-- A billing restore's destination holds none of the merchant's fee reads.
CREATE OR REPLACE FUNCTION billing.guard_billing_restore_receipt() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
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
                  'balance_watches',
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_limits',
                  'catalog_meters',
                  'catalog_product_limits',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'credit_grant_events',
                  'credit_grants',
                  'custodians',
                  'customer_delinquency',
                  'customer_invoice_profiles',
                  'customer_limit_overrides',
                  'customer_low_balance_lines',
                  'customer_messages',
                  'customer_mrr',
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
                  'disputes',
                  'eventlog_events',
                  'fraud_reports',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'lease_in_flight',
                  'leases',
                  'ledger_accounts',
                  'ledger_daily',
                  'ledger_rollup_pending',
                  'ledger_transfers',
                  'ledger_type_totals',
                  'limit_adjustments',
                  'limit_hold_parts',
                  'limit_holds',
                  'limit_windows',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'money_settings',
                  'mrr_cohort_flows',
                  'mrr_flows',
                  'mrr_movement_lines',
                  'mrr_movements',
                  'mrr_product_flows',
                  'mrr_scheduled',
                  'mrr_totals',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_events',
                  'operation_log',
                  'order_attempts',
                  'order_lines',
                  'orders',
                  'owed_items',
                  'ownership_claims',
                  'payment_attempts',
                  'payment_method_versions',
                  'payment_methods',
                  'payments',
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
                  'stripe_fee_reads',
                  'subscription_billing_changes',
                  'subscription_mrr',
                  'subscription_status_transitions',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_card_prints',
                  'usage_postings',
                  'usage_unposted',
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
$_$;
