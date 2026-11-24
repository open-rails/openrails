-- When a merchant's general ledger booked its first receipt, by the clock
-- that stamps payments and orders: the books' start. Recorded once, by the
-- payment writer that first books money; a later booking of an older payment
-- never moves it. ledger-audit checks only what was stored since.
CREATE TABLE billing.books_starts (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    started_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT books_starts_pkey PRIMARY KEY (merchant_id)
);
COMMENT ON TABLE billing.books_starts IS 'One row per merchant: when its general ledger booked its first receipt, by the clock that stamps payments and orders. ledger-audit checks the postings of what was stored since; what came before waits for the books'' rebuild. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.books_starts.started_at IS 'When the first receipt was booked, by the payment writer''s clock.';

CREATE TRIGGER immutable_books_starts BEFORE DELETE OR UPDATE ON billing.books_starts
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_books_starts_truncate BEFORE TRUNCATE ON billing.books_starts
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- The share of a credit lot each reversal took, or each recovery restored,
-- decided once when it first settles, nothing included: the lot's record of
-- what its reversals moved, whether or not the general ledger booked the lot.
CREATE TABLE billing.credit_reversal_shares (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    reversal_id uuid NOT NULL,
    credit_grant_id uuid NOT NULL,
    origin_id uuid NOT NULL,
    cash bigint NOT NULL CHECK (cash >= 0),
    units bigint NOT NULL CHECK (units >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT credit_reversal_shares_pkey PRIMARY KEY (merchant_id, reversal_id, credit_grant_id, origin_id),
    CONSTRAINT credit_reversal_shares_reversal_id_fkey FOREIGN KEY (merchant_id, reversal_id) REFERENCES billing.payments (merchant_id, id),
    CONSTRAINT credit_reversal_shares_origin_id_fkey FOREIGN KEY (merchant_id, origin_id) REFERENCES billing.payments (merchant_id, id),
    CONSTRAINT credit_reversal_shares_credit_grant_id_fkey FOREIGN KEY (merchant_id, credit_grant_id) REFERENCES billing.credit_grants (merchant_id, id)
);
CREATE INDEX credit_reversal_shares_credit_grant_id_idx ON billing.credit_reversal_shares (merchant_id, credit_grant_id, origin_id);
COMMENT ON TABLE billing.credit_reversal_shares IS 'Each settled reversal''s share of the paid credit lot its payment bought, decided when it first settled, zero included, and never changed: the record of the lot''s returned money and units. A refund or chargeback has one row, its own (origin_id = reversal_id); a recovery has one per chargeback it restored, or one of its own, zero, when it restored none. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.credit_reversal_shares.origin_id IS 'The reversal whose share this is: the row''s own, or the chargeback a recovery restored.';
COMMENT ON COLUMN billing.credit_reversal_shares.cash IS 'The lot''s money it took, or restored of origin_id''s, in the lot''s paid_currency.';
COMMENT ON COLUMN billing.credit_reversal_shares.units IS 'The lot''s units it took, or restored of origin_id''s, in the lot''s unit.';

-- Records the share of each of a merchant's settled reversals of a paid lot
-- that has none, as it was decided, from its legs in either book: a refund's
-- or chargeback's own; a recovery's per chargeback its labels name; nothing
-- when it has none, unless it was set aside (one of a unit pack the books
-- never booked, which the next pass decides). Units are every units-book leg
-- of the lot's face (not the cash, nor the difference of face and cash a lot
-- from before the credit books posted to promotion or revenue). This
-- migration and a restore of an archive from before the shares call it.
CREATE FUNCTION billing.seed_credit_reversal_shares(p_merchant uuid) RETURNS void
LANGUAGE sql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
    WITH legs AS (
        SELECT r.merchant_id, r.id AS reversal_id, g.id AS credit_grant_id,
               CASE WHEN r.amount > 0 THEN substring(lt.source FROM 'restore:([0-9a-f-]{36})$')::uuid ELSE r.id END AS origin_id,
               CASE WHEN lt.book = 'units' AND lt.transfer_type IN ('credit_refund_cash', 'credit_refund_cash_restore')
                      OR lt.book = 'general' AND (c.account_type IN ('cash_clearing', 'external_receipts') OR d.account_type IN ('cash_clearing', 'external_receipts'))
                    THEN lt.amount ELSE 0 END AS cash,
               CASE WHEN lt.book = 'units' AND lt.transfer_type IN ('credit_refund', 'credit_refund_restore', 'credit_refund_funding') AND lt.source NOT IN ('promotion', 'revenue')
                    THEN lt.amount ELSE 0 END AS units
        FROM billing.payments r
        JOIN billing.credit_grants g ON g.merchant_id = r.merchant_id AND g.payment_id = r.refunded_payment_id AND g.paid_amount IS NOT NULL
        JOIN billing.ledger_transfers lt ON lt.merchant_id = r.merchant_id AND lt.credit_grant_id = g.id AND lt.operation = 'purchased_credit_refund' AND lt.source_id = r.id::text
        JOIN billing.ledger_accounts d ON d.merchant_id = lt.merchant_id AND d.id = lt.debit_account_id
        JOIN billing.ledger_accounts c ON c.merchant_id = lt.merchant_id AND c.id = lt.credit_account_id
        WHERE r.merchant_id = p_merchant AND r.refunded_payment_id IS NOT NULL AND r.status = 'succeeded' AND r.amount <> 0 AND r.deleted_at IS NULL
          AND NOT EXISTS (SELECT 1 FROM billing.credit_reversal_shares s WHERE s.merchant_id = r.merchant_id AND s.reversal_id = r.id AND s.credit_grant_id = g.id)
    )
    INSERT INTO billing.credit_reversal_shares (merchant_id, reversal_id, credit_grant_id, origin_id, cash, units)
    SELECT merchant_id, reversal_id, credit_grant_id, origin_id, sum(cash)::bigint, sum(units)::bigint
    FROM legs WHERE origin_id IS NOT NULL
    GROUP BY merchant_id, reversal_id, credit_grant_id, origin_id;
    INSERT INTO billing.credit_reversal_shares (merchant_id, reversal_id, credit_grant_id, origin_id, cash, units)
    SELECT r.merchant_id, r.id, g.id, r.id, 0, 0
    FROM billing.payments r
    JOIN billing.credit_grants g ON g.merchant_id = r.merchant_id AND g.payment_id = r.refunded_payment_id AND g.paid_amount IS NOT NULL
    WHERE r.merchant_id = p_merchant AND r.refunded_payment_id IS NOT NULL AND r.status = 'succeeded' AND r.amount <> 0 AND r.deleted_at IS NULL
      AND NOT EXISTS (SELECT 1 FROM billing.credit_reversal_shares s WHERE s.merchant_id = r.merchant_id AND s.reversal_id = r.id AND s.credit_grant_id = g.id)
      AND (g.deferred_credited > 0 OR g.paid_currency = g.unit);
$$;
SELECT billing.seed_credit_reversal_shares(id) FROM billing.merchants;

-- What a lot's pending refunds reserve follows what its reversals returned,
-- as their shares record it.
CREATE OR REPLACE FUNCTION billing.credit_grant_reservation(p_merchant uuid, p_lot uuid) RETURNS bigint
LANGUAGE sql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
    SELECT COALESCE((
        SELECT LEAST(g.units, ceil(g.units::numeric * GREATEST(LEAST(pending.amount, g.paid_amount - returned.amount), 0) / g.paid_amount))::bigint
        FROM billing.credit_grants g
        CROSS JOIN LATERAL (SELECT sum(-r.amount)::numeric AS amount FROM billing.payments r
            WHERE r.merchant_id = g.merchant_id AND r.refunded_payment_id = g.payment_id AND r.currency = g.paid_currency
              AND r.status = 'pending' AND r.amount < 0 AND r.deleted_at IS NULL) pending
        CROSS JOIN LATERAL (SELECT COALESCE(sum(CASE WHEN s.origin_id = s.reversal_id THEN s.cash ELSE -s.cash END), 0)::numeric AS amount
            FROM billing.credit_reversal_shares s
            WHERE s.merchant_id = g.merchant_id AND s.credit_grant_id = g.id) returned
        WHERE g.merchant_id = p_merchant AND g.id = p_lot AND g.payment_id IS NOT NULL AND g.paid_amount IS NOT NULL
          AND pending.amount > 0), 0)
$$;

CREATE TRIGGER immutable_credit_reversal_shares BEFORE DELETE OR UPDATE ON billing.credit_reversal_shares
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_credit_reversal_shares_truncate BEFORE TRUNCATE ON billing.credit_reversal_shares
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- What of a schedule's recognised revenue an early end forfeited and no undone
-- end has taken back: all an undone end can reinstate. A reversal's contra
-- never becomes one.
ALTER TABLE billing.revenue_schedules
    ADD COLUMN forfeited bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT revenue_schedules_forfeited_check CHECK (forfeited >= 0);
UPDATE billing.revenue_schedules s
   SET forfeited = GREATEST(f.forfeited, 0)
  FROM (SELECT t.merchant_id, t.revenue_schedule_id,
               sum(CASE WHEN t.operation = 'termination' THEN t.amount ELSE -t.amount END) AS forfeited
          FROM billing.ledger_transfers t
         WHERE t.book = 'general' AND t.source = 'revenue_schedule' AND t.operation IN ('termination', 'reinstatement')
         GROUP BY t.merchant_id, t.revenue_schedule_id) f
 WHERE s.merchant_id = f.merchant_id AND s.id = f.revenue_schedule_id;
ALTER TABLE billing.revenue_schedules ALTER COLUMN forfeited DROP DEFAULT;
COMMENT ON COLUMN billing.revenue_schedules.forfeited IS 'What of recognized_net an early end forfeited and no undone end took back: the most an undone end reinstates.';

DROP TRIGGER immutable_revenue_schedule_facts ON billing.revenue_schedules;
CREATE TRIGGER immutable_revenue_schedule_facts BEFORE DELETE OR UPDATE ON billing.revenue_schedules
    FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('funded', 'recognized_net', 'ended_at', 'recognized_through_at', 'next_recognition_at', 'reinstatements', 'forfeited', 'updated_at');

-- The books' start and the lots' reversal shares are restored with the rest
-- of a merchant.
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
                  'catalog_restore_receipts',
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
                  'lease_in_flight',
                  'leases',
                  'ledger_accounts',
                  'ledger_daily',
                  'ledger_rollup_pending',
                  'ledger_transfers',
                  'ledger_type_totals',
                  'ledger_units',
                  'limit_adjustments',
                  'limit_hold_parts',
                  'limit_holds',
                  'limit_windows',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'merchant_leases',
                  'money_settings',
                  'mrr_cohort_flows',
                  'mrr_flows',
                  'mrr_movement_lines',
                  'mrr_movements',
                  'mrr_product_flows',
                  'mrr_risk_cases',
                  'mrr_risk_due',
                  'mrr_risk_episodes',
                  'mrr_risk_flows',
                  'mrr_risk_totals',
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
                  'revenue_schedules',
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
