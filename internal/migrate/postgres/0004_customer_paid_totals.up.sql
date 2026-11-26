-- Trust tiers graduate by what a customer paid. customer_paid_totals keeps,
-- per customer and currency, what it paid net of refunds and chargebacks and
-- when it first paid; the payments trigger writes it in the transaction that
-- settles, refunds or charges back a payment, and billing.trust_tier reads it
-- with the merchant's rules.

CREATE TABLE billing.customer_paid_totals (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    net_paid_amount bigint NOT NULL,
    first_paid_at timestamptz,
    PRIMARY KEY (merchant_id, customer_id, currency),
    CONSTRAINT customer_paid_totals_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id)
);
COMMENT ON TABLE billing.customer_paid_totals IS 'What a customer paid in a currency, net of refunds and chargebacks, and when it first paid: what the merchant''s trust tier rules read. Written only by the payments trigger, which rebuilds it on restore.';
COMMENT ON COLUMN billing.customer_paid_totals.net_paid_amount IS 'Settled receipts (books.ReceiptOf: taken at a rail on a PSP, or recorded by the merchant) plus the signed amounts of their settled refunds, chargebacks and won disputes, in native units.';
COMMENT ON COLUMN billing.customer_paid_totals.first_paid_at IS 'The earliest settled receipt''s purchased_at; NULL when none counts.';

-- A payment's part in its customer's paid totals: a settled receipt adds its
-- amount and may be the first payment; a settled refund, chargeback or won
-- dispute of a receipt adds its own signed amount. A receipt that stops
-- counting (pruned or deleted) hands first_paid_at to the next one.
CREATE FUNCTION billing.payment_paid_totals() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    sign integer;
    p record;
    receipt boolean;
BEGIN
    FOREACH sign IN ARRAY CASE TG_OP WHEN 'INSERT' THEN ARRAY[1] WHEN 'DELETE' THEN ARRAY[-1] ELSE ARRAY[-1, 1] END LOOP
        IF sign < 0 THEN p := OLD; ELSE p := NEW; END IF;
        CONTINUE WHEN p.deleted_at IS NOT NULL;
        receipt := p.reversal_kind IS NULL AND p.amount > 0 AND p.status IN ('succeeded', 'refunded')
            AND (p.channel = 'manual' OR p.money_movement = 'rail' AND p.psp_id IS NOT NULL);
        CONTINUE WHEN NOT receipt AND NOT (p.reversal_kind IS NOT NULL AND p.status = 'succeeded' AND p.amount <> 0
            AND EXISTS (SELECT 1 FROM billing.payments o WHERE o.merchant_id = p.merchant_id AND o.id = p.refunded_payment_id
                AND (o.channel = 'manual' OR o.money_movement = 'rail' AND o.psp_id IS NOT NULL)));
        INSERT INTO billing.customer_paid_totals AS t (merchant_id, customer_id, currency, net_paid_amount, first_paid_at)
        VALUES (p.merchant_id, p.customer_id, p.currency, sign * p.amount, CASE WHEN receipt AND sign > 0 THEN p.purchased_at END)
        ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE
            SET net_paid_amount = t.net_paid_amount + EXCLUDED.net_paid_amount, first_paid_at = LEAST(t.first_paid_at, EXCLUDED.first_paid_at);
        IF receipt AND sign < 0 THEN
            UPDATE billing.customer_paid_totals t SET first_paid_at = (
                SELECT min(o.purchased_at) FROM billing.payments o
                WHERE o.merchant_id = p.merchant_id AND o.customer_id = p.customer_id AND o.currency = p.currency
                  AND o.deleted_at IS NULL AND o.reversal_kind IS NULL AND o.amount > 0 AND o.status IN ('succeeded', 'refunded')
                  AND (o.channel = 'manual' OR o.money_movement = 'rail' AND o.psp_id IS NOT NULL))
            WHERE t.merchant_id = p.merchant_id AND t.customer_id = p.customer_id AND t.currency = p.currency
              AND t.first_paid_at = p.purchased_at;
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

-- The trigger first: its lock holds other writers of payments until the
-- totals below are in.
CREATE TRIGGER payments_paid_totals AFTER INSERT OR DELETE OR UPDATE OF status, amount, currency, deleted_at, purchased_at, money_movement, psp_id ON billing.payments
    FOR EACH ROW EXECUTE FUNCTION billing.payment_paid_totals();

INSERT INTO billing.customer_paid_totals (merchant_id, customer_id, currency, net_paid_amount, first_paid_at)
SELECT p.merchant_id, p.customer_id, p.currency, sum(p.amount), min(p.purchased_at) FILTER (WHERE p.reversal_kind IS NULL)
FROM billing.payments p
LEFT JOIN billing.payments o ON o.merchant_id = p.merchant_id AND o.id = p.refunded_payment_id
WHERE p.deleted_at IS NULL AND (
    p.reversal_kind IS NULL AND p.amount > 0 AND p.status IN ('succeeded', 'refunded')
        AND (p.channel = 'manual' OR p.money_movement = 'rail' AND p.psp_id IS NOT NULL)
    OR p.reversal_kind IS NOT NULL AND p.status = 'succeeded' AND p.amount <> 0
        AND (o.channel = 'manual' OR o.money_movement = 'rail' AND o.psp_id IS NOT NULL))
GROUP BY p.merchant_id, p.customer_id, p.currency;

-- A customer's trust tier in one currency: its explicit level, else the
-- highest tier whose rule its paid totals meet at p_now, else the default.
-- p_trust is the merchant's {default: tier, rules: {currency: [{tier,
-- net_paid_amount, days}]}}, each currency's highest tier first
-- (merchantconfig.Settings.TrustParam).
CREATE FUNCTION billing.trust_tier(p_trust jsonb, p_currency text, p_explicit text, p_net_paid bigint, p_first_paid_at timestamptz, p_now timestamptz) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT COALESCE(NULLIF(p_explicit, ''), (
    SELECT r.rule ->> 'tier'
    FROM jsonb_array_elements(p_trust -> 'rules' -> p_currency) WITH ORDINALITY AS r(rule, n)
    WHERE COALESCE(p_net_paid, 0) >= (r.rule ->> 'net_paid_amount')::bigint
      AND ((r.rule ->> 'days')::integer = 0 OR p_first_paid_at <= p_now - (r.rule ->> 'days')::integer * interval '24 hours')
    ORDER BY r.n
    LIMIT 1), p_trust ->> 'default')
$$;

-- The restore guard names the new table.
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
