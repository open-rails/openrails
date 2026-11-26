-- The balance is the customer's ledger account less pending usage: intake adds
-- each item's provisional price per lease, and postings (daily, before a usage
-- invoice, before a credit lot's expiry, revocation or refund) take it.

ALTER TABLE billing.usage_unposted RENAME TO usage_pending;
ALTER TABLE billing.usage_pending RENAME CONSTRAINT usage_unposted_pkey TO usage_pending_pkey;
ALTER TABLE billing.usage_pending RENAME CONSTRAINT usage_unposted_customer_id_fkey TO usage_pending_customer_id_fkey;
ALTER TABLE billing.usage_pending RENAME CONSTRAINT usage_unposted_due_at_check TO usage_pending_due_at_check;
ALTER TABLE billing.usage_pending RENAME CONSTRAINT usage_unposted_currency_check TO usage_pending_currency_check;
ALTER INDEX billing.usage_unposted_due_at_idx RENAME TO usage_pending_due_at_idx;
COMMENT ON TABLE billing.usage_pending IS 'A customer whose usage in a currency the ledger has not taken in full: the posting queue. Intake holds the row FOR KEY SHARE from before it inserts its events until it commits, and writes it only to move since_at or due_at earlier; a posting locks it FOR UPDATE, takes every usage the event store holds and deletes the customer''s usage_pending_amounts. A posting that leaves nothing to post deletes it.';
COMMENT ON COLUMN billing.usage_pending.since_at IS 'The start of the oldest pricing period whose usage may not be posted in full: a posting reads from it.';
COMMENT ON COLUMN billing.usage_pending.due_at IS 'When the daily posting takes it: the end of the merchant''s accounting-zone day its oldest pending usage occurred in, or when a period whose price fell may be credited.';

CREATE TABLE billing.usage_pending_amounts (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    lease_id uuid NOT NULL,
    amount bigint NOT NULL CHECK (amount > 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, currency, lease_id),
    CONSTRAINT usage_pending_amounts_pending_fkey FOREIGN KEY (merchant_id, customer_id, currency) REFERENCES billing.usage_pending (merchant_id, customer_id, currency) ON DELETE CASCADE
);
COMMENT ON TABLE billing.usage_pending_amounts IS 'The provisional price of a customer''s usage in a currency that no posting has taken, per lease that took it: the balance is the customer''s ledger account less their sum. A lease''s syncs serialize on the lease, so intakes never wait on each other here; a posting deletes the rows it took.';
COMMENT ON COLUMN billing.usage_pending_amounts.amount IS 'What the lease''s settled usage costs at the provisional prices intake gave it, in the currency''s native units.';

ALTER TABLE billing.usage_postings DROP CONSTRAINT usage_postings_reason_check;
UPDATE billing.usage_postings SET reason = CASE reason WHEN 'close' THEN 'daily' ELSE 'credit' END WHERE reason IN ('close', 'movement');
ALTER TABLE billing.usage_postings ADD CONSTRAINT usage_postings_reason_check CHECK (reason IN ('daily', 'billing', 'credit', 'purge', 'reversal'));
COMMENT ON COLUMN billing.usage_postings.cut_at IS 'Usage that occurred before it is in the posting: past every usage the event store held (now plus the usage clock skew), or a credit lot''s lapse.';
COMMENT ON COLUMN billing.usage_postings.reason IS 'daily: once the accounting-zone day of the oldest pending usage ended (the month-end close is its last day''s); billing: before a usage invoice is billed; credit: before a credit lot''s expiry, revocation or refund; purge: before a merchant purge deletes the event store''s usage; reversal: a period whose price fell below what posted, once its late usage window closed.';

DELETE FROM billing.lease_in_flight WHERE limit_key = 'balance';
ALTER TABLE billing.lease_in_flight DROP CONSTRAINT lease_in_flight_limit_key_check;
ALTER TABLE billing.lease_in_flight ADD CONSTRAINT lease_in_flight_limit_key_check
    CHECK (limit_key ~ '^[a-z][a-z0-9_-]{0,63}$' AND limit_key <> 'balance' OR limit_key ~ '^spend:[0-9a-f]{64}$');
COMMENT ON TABLE billing.lease_in_flight IS 'What a gate has had in flight for one customer''s declared limit or spend window across at least one sync: open tickets and usage a sync left unsettled. Other gates'' views and admissions count it as held in the limit''s current window while its lease is live; the balance is never reported. Retention: rows; each sync replaces its lease''s rows, close clears them, and they go with the lease.';
COMMENT ON COLUMN billing.lease_in_flight.limit_key IS 'A declared limit''s key, or a spend window''s spend:<identity>.';
COMMENT ON COLUMN billing.lease_in_flight.unit IS 'The limit''s unit.';

ALTER TABLE billing.credit_grants RENAME COLUMN units_reserved TO units_refunding;
ALTER TABLE billing.credit_grants RENAME CONSTRAINT credit_grants_units_reserved_check TO credit_grants_units_refunding_check;
DROP INDEX billing.credit_grants_reserved_idx;
CREATE INDEX credit_grants_refunding_idx ON billing.credit_grants (merchant_id, customer_id, unit)
    WHERE units_refunding > 0;
COMMENT ON COLUMN billing.credit_grants.units_refunding IS 'Units pending refunds of the lot''s payment withdrew from its balance (credit_refund at their own ids): off the balance at once, their money still deferred until each refund settles, or back on the balance (credit_refund_restore) when one fails. Kept by billing.credit_grant_refunding.';
DROP FUNCTION billing.credit_grant_reservation(uuid, uuid);

CREATE OR REPLACE FUNCTION billing.guard_credit_grant_facts() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF (NEW.units_credited <> 0 OR NEW.units_debited <> 0 OR NEW.units_refunding <> 0 OR NEW.revoked_at IS NOT NULL
            OR NEW.deferred_credited <> 0 OR NEW.deferred_debited <> 0)
           AND NOT billing.billing_restore_active(NEW.merchant_id) THEN
            RAISE EXCEPTION 'a credit grant starts with no posted, refunding, deferred or revoked units' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    -- Only a nested trigger moves the counters and revoked_at: the transfer
    -- and event inserts are their writers. units_refunding is state.
    IF TG_OP = 'DELETE'
       OR (to_jsonb(NEW) - ARRAY['units_credited', 'units_debited', 'units_refunding', 'revoked_at', 'deferred_credited', 'deferred_debited']) IS DISTINCT FROM
          (to_jsonb(OLD) - ARRAY['units_credited', 'units_debited', 'units_refunding', 'revoked_at', 'deferred_credited', 'deferred_debited'])
       OR (pg_trigger_depth() < 2 AND (NEW.units_credited, NEW.units_debited, NEW.revoked_at, NEW.deferred_credited, NEW.deferred_debited)
           IS DISTINCT FROM (OLD.units_credited, OLD.units_debited, OLD.revoked_at, OLD.deferred_credited, OLD.deferred_debited)) THEN
        RAISE EXCEPTION 'credit grant facts are immutable; its ledger transfers and events keep its counters' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

-- What a lot's pending refunds withdrew: their credit_refund legs, less what
-- a release restored.
CREATE FUNCTION billing.credit_grant_refunding(p_merchant uuid, p_lot uuid) RETURNS bigint
LANGUAGE sql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
    SELECT COALESCE(sum(CASE lt.transfer_type WHEN 'credit_refund' THEN lt.amount ELSE -lt.amount END), 0)::bigint
    FROM billing.credit_grants g
    JOIN billing.payments r ON r.merchant_id = g.merchant_id AND r.refunded_payment_id = g.payment_id
        AND r.status = 'pending' AND r.amount < 0 AND r.deleted_at IS NULL
    JOIN billing.ledger_transfers lt ON lt.merchant_id = g.merchant_id AND lt.credit_grant_id = g.id AND lt.book = 'units'
        AND lt.operation = 'purchased_credit_refund' AND lt.source_id = r.id::text
        AND lt.transfer_type IN ('credit_refund', 'credit_refund_restore')
    WHERE g.merchant_id = p_merchant AND g.id = p_lot AND g.payment_id IS NOT NULL
$$;

UPDATE billing.credit_grants SET units_refunding = billing.credit_grant_refunding(merchant_id, id) WHERE units_refunding <> 0;

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
                  'books_starts',
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_limits',
                  'catalog_meters',
                  'catalog_product_limits',
                  'catalog_rate_cards',
                  'credit_grant_events',
                  'credit_grants',
                  'credit_reversal_shares',
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
                  'invoice_attempts',
                  'invoice_lines',
                  'invoices',
                  'lease_in_flight',
                  'leases',
                  'ledger_accounts',
                  'ledger_daily',
                  'ledger_rollup_pending',
                  'ledger_transfers',
                  'ledger_type_totals',
                  'ledger_units',
                  'limit_adjustments',
                  'limit_windows',
                  'maintenance_runs',
                  'mandates',
                  'merchant_leases',
                  'money_settings',
                  'mrr_cohort_flows',
                  'mrr_customer_flows',
                  'mrr_customer_totals',
                  'mrr_flows',
                  'mrr_movement_lines',
                  'mrr_movements',
                  'mrr_product_flows',
                  'mrr_product_totals',
                  'mrr_risk_cases',
                  'mrr_risk_due',
                  'mrr_risk_episodes',
                  'mrr_risk_flows',
                  'mrr_risk_totals',
                  'mrr_scheduled',
                  'mrr_streak_flows',
                  'mrr_totals',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_events',
                  'operation_log',
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
                  'provider_listed_rebills',
                  'provider_schedules',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'revenue_schedules',
                  'running_charges',
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'stored_card_checks',
                  'stripe_fee_reads',
                  'subscription_billing_changes',
                  'subscription_cancellations',
                  'subscription_mrr',
                  'subscription_status_transitions',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_card_prints',
                  'usage_pending',
                  'usage_pending_amounts',
                  'usage_postings',
                  'webhook_events',
                  'webhook_health',
                  'webhook_health_daily']::text[])
        LOOP
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.%I WHERE merchant_id=$1)',TG_TABLE_SCHEMA,item.relname)
                INTO occupied USING NEW.merchant_id;
            IF occupied THEN RAISE EXCEPTION 'billing restore destination is not empty: %',item.relname USING ERRCODE='55000'; END IF;
        END LOOP;
        IF EXISTS (SELECT 1 FROM billing.merchant_controls c WHERE c.merchant_id=NEW.merchant_id
            AND (c.destructive_armed OR c.first_pull_completed_at IS NOT NULL)) THEN
            RAISE EXCEPTION 'billing restore destination is not empty: merchant_controls' USING ERRCODE='55000';
        END IF;
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
