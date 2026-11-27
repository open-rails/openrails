-- A posting prices each pricing period from PostgreSQL's running totals
-- (usage_totals) less what the period's earlier usage receipts took, and
-- writes one usage receipt (usage_postings, renamed) itemized by meter. Intake
-- writes the totals in the transaction that settles its usage: they are the
-- next item's tier position and the pending amount, so no money path reads
-- the event store's rollups. Usage settled and not yet posted when a database
-- upgrades has no quantities here and is not charged.

ALTER TABLE billing.usage_postings RENAME TO usage_receipts;
ALTER TABLE billing.usage_receipts DROP COLUMN ingested_through;
ALTER TABLE billing.usage_receipts RENAME CONSTRAINT usage_postings_pkey TO usage_receipts_pkey;
ALTER TABLE billing.usage_receipts RENAME CONSTRAINT usage_postings_amount_check TO usage_receipts_amount_check;
ALTER TABLE billing.usage_receipts RENAME CONSTRAINT usage_postings_currency_check TO usage_receipts_currency_check;
ALTER TABLE billing.usage_receipts RENAME CONSTRAINT usage_postings_customer_id_fkey TO usage_receipts_customer_id_fkey;
ALTER TABLE billing.usage_receipts RENAME CONSTRAINT usage_postings_forgiven_check TO usage_receipts_forgiven_check;
ALTER TABLE billing.usage_receipts RENAME CONSTRAINT usage_postings_forgiven_lines_check TO usage_receipts_forgiven_lines_check;
ALTER TABLE billing.usage_receipts RENAME CONSTRAINT usage_postings_lines_check TO usage_receipts_lines_check;
ALTER TABLE billing.usage_receipts RENAME CONSTRAINT usage_postings_merchant_id_fkey TO usage_receipts_merchant_id_fkey;
ALTER TABLE billing.usage_receipts RENAME CONSTRAINT usage_postings_period_ends_at_check TO usage_receipts_period_ends_at_check;
ALTER TABLE billing.usage_receipts RENAME CONSTRAINT usage_postings_reason_check TO usage_receipts_reason_check;
ALTER INDEX billing.usage_postings_forgiven_idx RENAME TO usage_receipts_forgiven_idx;
ALTER INDEX billing.usage_postings_period_idx RENAME TO usage_receipts_period_idx;
COMMENT ON TABLE billing.usage_receipts IS 'What one posting took of a customer''s usage in one pricing period, never changed once written: the price of the period''s settled quantity less what its earlier receipts took, itemized by meter. The posting draws the lots first, one credit_spend each, and leaves the rest owed in one owed_accrual, at (usage_posting, usage_receipt:<id>, 1); a reversal credits a period whose price fell, as usage_reversed. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.usage_receipts.cut_at IS 'The receipt took the settled usage that occurred before it: a credit lot''s lapse, else all of it (the posting''s instant plus the usage clock skew).';
COMMENT ON COLUMN billing.usage_receipts.reason IS 'daily: once the accounting-zone day of the oldest pending usage ended; billing: before a usage invoice is billed; credit: before a credit lot''s expiry, revocation or refund, or an invoice paid from the balance; purge: before a merchant purge; reversal: the period''s price fell below what its receipts took (a volume tier).';
COMMENT ON COLUMN billing.usage_receipts.amount IS 'What the posting took: positive for usage, negative for a reversal.';
COMMENT ON COLUMN billing.usage_receipts.lines IS 'Per meter, cell and card print, what the posting added: [{meter_key, cell, print, requests, quantity, stated, amount}]. A period''s receipts sum to the price of its settled usage.';
COMMENT ON COLUMN billing.usage_receipts.forgiven IS 'Of amount, what the ledger did not take: usage in a merchant''s unit its lots could not cover, never owed (positive); a reversal''s share that took back forgiveness rather than credit (negative). Zero in a registry currency.';
COMMENT ON COLUMN billing.usage_receipts.forgiven_lines IS 'How forgiven is shared, as the posting computed it: [{invoker, event_type, meter_key, cell, amount}], by what the period charged each before the cut, to the largest remainder; a reversal takes back from the period''s shares the same way. NULL when nothing was forgiven.';

-- The running totals: what intake settled, per lease until a posting takes
-- it, then one posted row per period, meter cell and card print.
CREATE TABLE billing.usage_totals (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    period_starts_at timestamptz NOT NULL,
    period_ends_at timestamptz NOT NULL,
    meter_key text NOT NULL CHECK (btrim(meter_key) <> ''),
    cell text NOT NULL,
    print text NOT NULL,
    lapse_at timestamptz NOT NULL,
    lease_id uuid NOT NULL,
    quantity bigint NOT NULL CHECK (quantity >= 0),
    stated bigint NOT NULL CHECK (stated >= 0),
    requests bigint NOT NULL CHECK (requests > 0),
    amount bigint NOT NULL CHECK (amount >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, currency, period_starts_at, meter_key, cell, print, lapse_at, lease_id),
    CONSTRAINT usage_totals_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT usage_totals_print_fkey FOREIGN KEY (merchant_id, print) REFERENCES billing.usage_card_prints (merchant_id, print),
    CONSTRAINT usage_totals_period_check CHECK (period_ends_at > period_starts_at AND lapse_at >= period_starts_at AND lapse_at < period_ends_at),
    -- A posted row (no lease) is what postings took: nothing of it is pending.
    CONSTRAINT usage_totals_posted_check CHECK (lease_id <> '00000000-0000-0000-0000-000000000000' OR amount = 0 AND lapse_at = period_starts_at)
);
COMMENT ON TABLE billing.usage_totals IS 'A customer''s settled usage of one meter cell under one card print in one pricing period: the next item''s tier position, pending usage and what a posting prices. Intake adds to its lease''s row (only that lease''s syncs write it, so gates never wait on each other); a posting folds the rows it took into the posted row (lease_id the nil UUID) and deletes them. Retention: rows are deleted a day after their period''s late usage window closes (36 days after period_ends_at), once none of the period''s rows is unposted.';
COMMENT ON COLUMN billing.usage_totals.cell IS 'The meter''s matrix cell; empty for a meter without one (part of the key).';
COMMENT ON COLUMN billing.usage_totals.lapse_at IS 'The latest lapse at or before the usage occurred of a credit lot the customer held with units left, else the period''s start: a posting before a lot''s lapse takes the rows from before it.';
COMMENT ON COLUMN billing.usage_totals.lease_id IS 'The lease whose syncs settled the usage, until a posting takes it; the nil UUID for what postings took.';
COMMENT ON COLUMN billing.usage_totals.amount IS 'Pending: what the lease''s settled usage costs at the provisional prices intake gave it, never below zero; zero once posted.';

CREATE INDEX usage_totals_closed_idx ON billing.usage_totals (merchant_id, period_ends_at)
    WHERE lease_id = '00000000-0000-0000-0000-000000000000';

-- Periods still open take their tier positions from what posted.
INSERT INTO billing.usage_totals (merchant_id, customer_id, currency, period_starts_at, period_ends_at, meter_key, cell, print, lapse_at, lease_id,
    quantity, stated, requests, amount, updated_at)
SELECT r.merchant_id, r.customer_id, r.currency, r.period_starts_at, max(r.period_ends_at), l.meter_key, l.cell, l.print, r.period_starts_at,
       '00000000-0000-0000-0000-000000000000'::uuid, sum(l.quantity), sum(l.stated), sum(l.requests), 0, now()
FROM billing.usage_receipts r
CROSS JOIN LATERAL jsonb_to_recordset(r.lines) AS l(meter_key text, cell text, print text, requests bigint, quantity bigint, stated bigint)
WHERE r.period_ends_at + interval '36 days' > now()
  AND EXISTS (SELECT 1 FROM billing.usage_card_prints p WHERE p.merchant_id = r.merchant_id AND p.print = l.print)
GROUP BY r.merchant_id, r.customer_id, r.currency, r.period_starts_at, l.meter_key, l.cell, l.print
HAVING sum(l.requests) > 0 AND sum(l.quantity) >= 0 AND sum(l.stated) >= 0;

-- Pending is the totals' unposted amounts; the queue keeps only when its
-- daily posting is due.
DROP TABLE billing.usage_pending_amounts;
ALTER TABLE billing.usage_pending DROP CONSTRAINT usage_pending_due_at_check;
ALTER TABLE billing.usage_pending DROP COLUMN since_at;
COMMENT ON TABLE billing.usage_pending IS 'A customer with usage in a currency no posting has taken: the posting queue. Intake holds the row FOR KEY SHARE from before it writes its usage until it commits, and writes it only to move due_at earlier; a posting locks it FOR UPDATE, so it waits for every intake of the customer, and deletes it once nothing is unposted.';
COMMENT ON COLUMN billing.usage_pending.due_at IS 'When the daily posting takes it: the end of the merchant''s accounting-zone day its oldest unposted usage occurred in.';

-- The restore guard names the new tables.
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
                  'customer_paid_totals',
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
                  'usage_receipts',
                  'usage_totals',
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
