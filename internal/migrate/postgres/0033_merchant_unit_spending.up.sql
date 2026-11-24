-- A merchant's own units (credits, tokens, an in-game currency) are bought
-- with money, given and spent. They are prepaid, never owed: usage its lots
-- cannot take is forgiven; an order in a unit is paid from the customer's
-- balance, never by a payment; and what a pack of a unit cost is money, in a
-- registry currency.

-- The registry's currencies (internal/currency); every other code a balance
-- may count is a merchant's unit.
ALTER TABLE billing.orders DROP CONSTRAINT orders_complete_payment_check;
ALTER TABLE billing.orders ADD CONSTRAINT orders_complete_payment_check CHECK (status <> 'complete' OR total = 0 OR payment_id IS NOT NULL OR origin = 'usage' OR collection_owner <> 'engine'
    OR currency NOT IN ('AED', 'AUD', 'BRL', 'CAD', 'CHF', 'CNY', 'CZK', 'DKK', 'EUR', 'GBP', 'HKD', 'ILS', 'INR', 'JPY', 'KRW', 'MXN', 'MYR', 'NOK', 'NZD', 'PHP', 'PLN', 'PYUSD', 'SAR', 'SEK', 'SGD', 'SOL', 'THB', 'TRY', 'USD', 'USD1', 'USDC', 'USDG', 'USDT', 'ZAR'));
COMMENT ON CONSTRAINT orders_complete_payment_check ON billing.orders IS 'A complete order was paid by its payment, unless it was free, is a usage order, is collected by a provider, or is in a merchant''s unit: the customer''s balance paid it, in credit_spend legs naming the order.';

ALTER TABLE billing.ledger_accounts ADD CONSTRAINT ledger_accounts_owed_currency_check CHECK (account_type <> 'customer_owed'
    OR currency IN ('AED', 'AUD', 'BRL', 'CAD', 'CHF', 'CNY', 'CZK', 'DKK', 'EUR', 'GBP', 'HKD', 'ILS', 'INR', 'JPY', 'KRW', 'MXN', 'MYR', 'NOK', 'NZD', 'PHP', 'PLN', 'PYUSD', 'SAR', 'SEK', 'SGD', 'SOL', 'THB', 'TRY', 'USD', 'USD1', 'USDC', 'USDG', 'USDT', 'ZAR'));
COMMENT ON CONSTRAINT ledger_accounts_owed_currency_check ON billing.ledger_accounts IS 'Nothing is owed in a merchant''s unit: an arrears account holds a registry currency.';

ALTER TABLE billing.credit_grants ADD CONSTRAINT credit_grants_paid_currency_registry_check CHECK (paid_currency IS NULL
    OR paid_currency IN ('AED', 'AUD', 'BRL', 'CAD', 'CHF', 'CNY', 'CZK', 'DKK', 'EUR', 'GBP', 'HKD', 'ILS', 'INR', 'JPY', 'KRW', 'MXN', 'MYR', 'NOK', 'NZD', 'PHP', 'PLN', 'PYUSD', 'SAR', 'SEK', 'SGD', 'SOL', 'THB', 'TRY', 'USD', 'USD1', 'USDC', 'USDG', 'USDT', 'ZAR'));
COMMENT ON COLUMN billing.credit_grants.paid_amount IS 'What was paid for the lot, in paid_currency (a registry currency); NULL for a gift. A pack of a merchant''s unit is paid in any currency: its rate is paid_amount over units, bonus units included.';

-- A posting of usage in a merchant's unit forgives what the lots cannot take,
-- and a reversal takes back forgiveness before it credits.
ALTER TABLE billing.usage_postings ADD COLUMN forgiven bigint NOT NULL DEFAULT 0;
ALTER TABLE billing.usage_postings ALTER COLUMN forgiven DROP DEFAULT;
ALTER TABLE billing.usage_postings ADD CONSTRAINT usage_postings_forgiven_check CHECK (forgiven = 0
    OR currency NOT IN ('AED', 'AUD', 'BRL', 'CAD', 'CHF', 'CNY', 'CZK', 'DKK', 'EUR', 'GBP', 'HKD', 'ILS', 'INR', 'JPY', 'KRW', 'MXN', 'MYR', 'NOK', 'NZD', 'PHP', 'PLN', 'PYUSD', 'SAR', 'SEK', 'SGD', 'SOL', 'THB', 'TRY', 'USD', 'USD1', 'USDC', 'USDG', 'USDT', 'ZAR')
    AND (reason <> 'reversal' AND forgiven > 0 AND forgiven <= amount OR reason = 'reversal' AND forgiven < 0 AND forgiven >= amount));
COMMENT ON COLUMN billing.usage_postings.forgiven IS 'Of amount, what the ledger did not take: usage in a merchant''s unit its lots could not cover, never owed (positive); a reversal''s share that took back forgiveness rather than credit (negative). Zero in a registry currency.';
ALTER TABLE billing.usage_postings ADD COLUMN forgiven_lines jsonb,
    ADD CONSTRAINT usage_postings_forgiven_lines_check CHECK ((forgiven = 0) = (forgiven_lines IS NULL)
        AND (forgiven_lines IS NULL OR jsonb_typeof(forgiven_lines) = 'array' AND octet_length(forgiven_lines::text) <= 262144));
COMMENT ON COLUMN billing.usage_postings.forgiven_lines IS 'How forgiven is shared, as the posting computed it: [{invoker, event_type, meter_key, cell, amount}], by what the period charged each before the cut, to the largest remainder; a reversal takes back from the period''s shares the same way. NULL when nothing was forgiven.';
CREATE INDEX usage_postings_forgiven_idx ON billing.usage_postings (merchant_id, cut_at) WHERE forgiven <> 0;

-- Which of a merchant's own units the units book counts, at the scale every
-- amount in it was written at: a configuration that later rescales or
-- removes one is caught at load, after a restart too.
CREATE TABLE billing.ledger_units (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    unit text NOT NULL CHECK (unit ~ '^[A-Z][A-Z0-9]{3,11}$' AND unit NOT IN ('AED', 'AUD', 'BRL', 'CAD', 'CHF', 'CNY', 'CZK', 'DKK', 'EUR', 'GBP', 'HKD', 'ILS', 'INR', 'JPY', 'KRW', 'MXN', 'MYR', 'NOK', 'NZD', 'PHP', 'PLN', 'PYUSD', 'SAR', 'SEK', 'SGD', 'SOL', 'THB', 'TRY', 'USD', 'USD1', 'USDC', 'USDG', 'USDT', 'ZAR')),
    scale integer NOT NULL CHECK (scale >= 0 AND scale <= 6),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, unit)
);
COMMENT ON TABLE billing.ledger_units IS 'A merchant''s own unit as the units book first counted it: written when a catalog write first names it or the book first holds it, under the units lock a settings change takes, with the scale its document in the file or Vault then declared. Loading a configuration that rescales or removes it serves this scale instead, with a finding. Retention: permanent, never pruned.';
CREATE TRIGGER immutable_ledger_units BEFORE DELETE OR UPDATE ON billing.ledger_units FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_ledger_units_truncate BEFORE TRUNCATE ON billing.ledger_units FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

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
