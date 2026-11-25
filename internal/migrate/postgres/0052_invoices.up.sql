-- An order is an invoice: the tables, their columns, keys, indexes, triggers,
-- sequence and functions take the invoice names, and mutable stored values
-- readers match on are rewritten. Immutable history (notification events,
-- destructive actions, the event log) keeps its values. Keys matched against
-- earlier records (idempotency keys, River kinds, ledger and books
-- coordinates, finding subjects, provider payload and metadata keys, NMI order
-- ids) keep their spelling.

ALTER TABLE billing.orders RENAME TO invoices;
ALTER TABLE billing.order_lines RENAME TO invoice_lines;
ALTER TABLE billing.order_attempts RENAME TO invoice_attempts;
ALTER SEQUENCE billing.order_number_seq RENAME TO invoice_number_seq;

-- Columns.
ALTER TABLE billing.credit_grants RENAME COLUMN order_line_id TO invoice_line_id;
ALTER TABLE billing.customer_delinquency RENAME COLUMN overdue_orders TO overdue_invoices;
ALTER TABLE billing.invoice_attempts RENAME COLUMN order_id TO invoice_id;
ALTER TABLE billing.invoice_lines RENAME COLUMN order_id TO invoice_id;
ALTER TABLE billing.invoices RENAME COLUMN checkout_order_id TO checkout_invoice_id;
ALTER TABLE billing.ledger_transfers RENAME COLUMN order_id TO invoice_id;
ALTER TABLE billing.ledger_transfers RENAME COLUMN order_line_id TO invoice_line_id;
ALTER TABLE billing.owed_items RENAME COLUMN order_id TO invoice_id;
ALTER TABLE billing.ownership_claims RENAME COLUMN order_id TO invoice_id;
ALTER TABLE billing.payment_attempts RENAME COLUMN order_id TO invoice_id;
ALTER TABLE billing.payments RENAME COLUMN order_id TO invoice_id;
ALTER TABLE billing.provider_bills RENAME COLUMN order_id TO invoice_id;
ALTER TABLE billing.provider_schedules RENAME COLUMN initial_order_id TO initial_invoice_id;
ALTER TABLE billing.revenue_schedules RENAME COLUMN order_id TO invoice_id;
ALTER TABLE billing.revenue_schedules RENAME COLUMN order_line_id TO invoice_line_id;
ALTER TABLE billing.solana_pay_receipts RENAME COLUMN order_attempt_id TO invoice_attempt_id;
ALTER TABLE billing.solana_pay_references RENAME COLUMN order_attempt_id TO invoice_attempt_id;
ALTER TABLE billing.subscriptions RENAME COLUMN checkout_order_id TO checkout_invoice_id;

-- Constraints (a key renames its index with it).
ALTER TABLE billing.credit_grants RENAME CONSTRAINT credit_grants_order_line_id_fkey TO credit_grants_invoice_line_id_fkey;
ALTER TABLE billing.invoice_attempts RENAME CONSTRAINT order_attempts_currency_check TO invoice_attempts_currency_check;
ALTER TABLE billing.invoice_attempts RENAME CONSTRAINT order_attempts_customer_id_fkey TO invoice_attempts_customer_id_fkey;
ALTER TABLE billing.invoice_attempts RENAME CONSTRAINT order_attempts_customer_id_payment_id_fkey TO invoice_attempts_customer_id_payment_id_fkey;
ALTER TABLE billing.invoice_attempts RENAME CONSTRAINT order_attempts_customer_id_subscription_id_fkey TO invoice_attempts_customer_id_subscription_id_fkey;
ALTER TABLE billing.invoice_attempts RENAME CONSTRAINT order_attempts_destructive_run_id_destructive_run_class_fkey TO invoice_attempts_destructive_run_id_destructive_run_class_fkey;
ALTER TABLE billing.invoice_attempts RENAME CONSTRAINT order_attempts_merchant_id_fkey TO invoice_attempts_merchant_id_fkey;
ALTER TABLE billing.invoice_attempts RENAME CONSTRAINT order_attempts_order_id_fkey TO invoice_attempts_invoice_id_fkey;
ALTER TABLE billing.invoice_attempts RENAME CONSTRAINT order_attempts_order_terms_check TO invoice_attempts_invoice_terms_check;
ALTER TABLE billing.invoice_attempts RENAME CONSTRAINT order_attempts_pkey TO invoice_attempts_pkey;
ALTER TABLE billing.invoice_attempts RENAME CONSTRAINT order_attempts_psp_id_rail_fkey TO invoice_attempts_psp_id_rail_fkey;
ALTER TABLE billing.invoice_attempts RENAME CONSTRAINT order_attempts_status_check TO invoice_attempts_status_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_access_duration_hours_check TO invoice_lines_access_duration_hours_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_amount_check TO invoice_lines_amount_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_billing_interval_hours_check TO invoice_lines_billing_interval_hours_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_calendar_cadence_check TO invoice_lines_calendar_cadence_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_claim_key_check TO invoice_lines_claim_key_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_description_check TO invoice_lines_description_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_order_id_fkey TO invoice_lines_invoice_id_fkey;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_order_id_position_key TO invoice_lines_invoice_id_position_key;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_ownership_check TO invoice_lines_ownership_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_pkey TO invoice_lines_pkey;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_position_check TO invoice_lines_position_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_price_id_fkey TO invoice_lines_price_id_fkey;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_product_access_id_fkey TO invoice_lines_product_access_id_fkey;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_product_id_fkey TO invoice_lines_product_id_fkey;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_provider_access_choice_check TO invoice_lines_provider_access_choice_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_quantified_check TO invoice_lines_quantified_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_quantity_check TO invoice_lines_quantity_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_seats_check TO invoice_lines_seats_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_source_check TO invoice_lines_source_check;
ALTER TABLE billing.invoice_lines RENAME CONSTRAINT order_lines_subscription_id_fkey TO invoice_lines_subscription_id_fkey;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_attempt_id_fkey TO invoices_attempt_id_fkey;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_attribution_check TO invoices_attribution_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_billing_contacts_check TO invoices_billing_contacts_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_cancel_url_check TO invoices_cancel_url_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_canceled_check TO invoices_canceled_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_checkout_facts_check TO invoices_checkout_facts_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_checkout_order_check TO invoices_checkout_invoice_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_checkout_order_id_fkey TO invoices_checkout_invoice_id_fkey;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_collection_method_check TO invoices_collection_method_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_collection_owner_check TO invoices_collection_owner_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_complete_check TO invoices_complete_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_complete_payment_check TO invoices_complete_payment_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_country_check TO invoices_country_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_currency_check TO invoices_currency_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_customer_id_fkey TO invoices_customer_id_fkey;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_customer_id_id_key TO invoices_customer_id_id_key;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_customer_type_check TO invoices_customer_type_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_expired_check TO invoices_expired_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_idempotency_key_check TO invoices_idempotency_key_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_language_check TO invoices_language_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_lines_digest_check TO invoices_lines_digest_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_memo_check TO invoices_memo_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_merchant_id_fkey TO invoices_merchant_id_fkey;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_number_check TO invoices_number_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_origin_check TO invoices_origin_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_pay_url_check TO invoices_pay_url_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_payment_id_fkey TO invoices_payment_id_fkey;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_payment_method_id_fkey TO invoices_payment_method_id_fkey;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_payment_status_check TO invoices_payment_status_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_period_check TO invoices_period_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_pkey TO invoices_pkey;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_po_number_check TO invoices_po_number_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_psp_id_fkey TO invoices_psp_id_fkey;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_purchase_number_check TO invoices_purchase_number_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_request_digest_check TO invoices_request_digest_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_status_check TO invoices_status_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_submitted_check TO invoices_submitted_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_subscription_bill_kind_check TO invoices_subscription_bill_kind_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_subscription_id_fkey TO invoices_subscription_id_fkey;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_succeeded_check TO invoices_succeeded_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_success_url_check TO invoices_success_url_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_tax_check TO invoices_tax_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_total_check TO invoices_total_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_uncollectible_check TO invoices_uncollectible_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_usage_check TO invoices_usage_check;
ALTER TABLE billing.invoices RENAME CONSTRAINT orders_void_check TO invoices_void_check;
ALTER TABLE billing.owed_items RENAME CONSTRAINT owed_items_order_id_fkey TO owed_items_invoice_id_fkey;
ALTER TABLE billing.ownership_claims RENAME CONSTRAINT ownership_claims_order_id_fkey TO ownership_claims_invoice_id_fkey;
ALTER TABLE billing.payment_attempts RENAME CONSTRAINT payment_attempts_order_id_fkey TO payment_attempts_invoice_id_fkey;
ALTER TABLE billing.payments RENAME CONSTRAINT payments_order_id_fkey TO payments_invoice_id_fkey;
ALTER TABLE billing.payments RENAME CONSTRAINT payments_order_required TO payments_invoice_required;
ALTER TABLE billing.provider_bills RENAME CONSTRAINT provider_bills_order_id_fkey TO provider_bills_invoice_id_fkey;
ALTER TABLE billing.provider_schedules RENAME CONSTRAINT provider_schedules_merchant_id_initial_order_id_fkey TO provider_schedules_merchant_id_initial_invoice_id_fkey;
ALTER TABLE billing.revenue_schedules RENAME CONSTRAINT revenue_schedules_order_id_fkey TO revenue_schedules_invoice_id_fkey;
ALTER TABLE billing.revenue_schedules RENAME CONSTRAINT revenue_schedules_order_line_id_fkey TO revenue_schedules_invoice_line_id_fkey;
ALTER TABLE billing.revenue_schedules RENAME CONSTRAINT revenue_schedules_order_line_id_key TO revenue_schedules_invoice_line_id_key;
ALTER TABLE billing.solana_pay_receipts RENAME CONSTRAINT solana_pay_receipts_order_attempt_id_fkey TO solana_pay_receipts_invoice_attempt_id_fkey;
ALTER TABLE billing.solana_pay_references RENAME CONSTRAINT solana_pay_references_order_attempt_id_fkey TO solana_pay_references_invoice_attempt_id_fkey;
ALTER TABLE billing.solana_pay_references RENAME CONSTRAINT solana_pay_references_order_attempt_id_key TO solana_pay_references_invoice_attempt_id_key;
ALTER TABLE billing.subscriptions RENAME CONSTRAINT subscriptions_checkout_order_id_fkey TO subscriptions_checkout_invoice_id_fkey;

-- Indexes.
ALTER INDEX billing.order_attempts_customer_id_idx RENAME TO invoice_attempts_customer_id_idx;
ALTER INDEX billing.order_attempts_destructive_run_id_idx RENAME TO invoice_attempts_destructive_run_id_idx;
ALTER INDEX billing.order_attempts_expires_at_abandoned_idx RENAME TO invoice_attempts_expires_at_abandoned_idx;
ALTER INDEX billing.order_attempts_expires_at_expirable_idx RENAME TO invoice_attempts_expires_at_expirable_idx;
ALTER INDEX billing.order_attempts_order_id_idx RENAME TO invoice_attempts_invoice_id_idx;
ALTER INDEX billing.order_attempts_order_id_live_key RENAME TO invoice_attempts_invoice_id_live_key;
ALTER INDEX billing.order_attempts_payment_id_idx RENAME TO invoice_attempts_payment_id_idx;
ALTER INDEX billing.order_attempts_psp_id_idx RENAME TO invoice_attempts_psp_id_idx;
ALTER INDEX billing.order_attempts_psp_id_reference_key RENAME TO invoice_attempts_psp_id_reference_key;
ALTER INDEX billing.order_attempts_psp_id_transaction_id_key RENAME TO invoice_attempts_psp_id_transaction_id_key;
ALTER INDEX billing.order_attempts_subscription_id_idx RENAME TO invoice_attempts_subscription_id_idx;
ALTER INDEX billing.order_attempts_transaction_id_key RENAME TO invoice_attempts_transaction_id_key;
ALTER INDEX billing.order_lines_price_id_idx RENAME TO invoice_lines_price_id_idx;
ALTER INDEX billing.order_lines_subscription_id_idx RENAME TO invoice_lines_subscription_id_idx;
ALTER INDEX billing.orders_checkout_created_at_idx RENAME TO invoices_checkout_created_at_idx;
ALTER INDEX billing.orders_checkout_order_id_idx RENAME TO invoices_checkout_invoice_id_idx;
ALTER INDEX billing.orders_closed_at_unpaid_idx RENAME TO invoices_closed_at_unpaid_idx;
ALTER INDEX billing.orders_created_at_id_idx RENAME TO invoices_created_at_id_idx;
ALTER INDEX billing.orders_customer_id_created_at_id_idx RENAME TO invoices_customer_id_created_at_id_idx;
ALTER INDEX billing.orders_customer_purchases_idx RENAME TO invoices_customer_purchases_idx;
ALTER INDEX billing.orders_due_at_open_idx RENAME TO invoices_due_at_open_idx;
ALTER INDEX billing.orders_expires_at_live_idx RENAME TO invoices_expires_at_live_idx;
ALTER INDEX billing.orders_id_idx RENAME TO invoices_id_idx;
ALTER INDEX billing.orders_idempotency_key_key RENAME TO invoices_idempotency_key_key;
ALTER INDEX billing.orders_number_key RENAME TO invoices_number_key;
ALTER INDEX billing.orders_open_lines_key RENAME TO invoices_open_lines_key;
ALTER INDEX billing.orders_pay_url_secret_hash_key RENAME TO invoices_pay_url_secret_hash_key;
ALTER INDEX billing.orders_payment_method_id_idx RENAME TO invoices_payment_method_id_idx;
ALTER INDEX billing.orders_renewal_key RENAME TO invoices_renewal_key;
ALTER INDEX billing.orders_sale_completed_at_idx RENAME TO invoices_sale_completed_at_idx;
ALTER INDEX billing.orders_subscription_period_idx RENAME TO invoices_subscription_period_idx;
ALTER INDEX billing.orders_usage_collection_due_idx RENAME TO invoices_usage_collection_due_idx;
ALTER INDEX billing.orders_usage_open_idx RENAME TO invoices_usage_open_idx;
ALTER INDEX billing.orders_usage_period_key RENAME TO invoices_usage_period_key;
ALTER INDEX billing.owed_items_order_id_idx RENAME TO owed_items_invoice_id_idx;
ALTER INDEX billing.ownership_claims_order_id_idx RENAME TO ownership_claims_invoice_id_idx;
ALTER INDEX billing.payment_attempts_order_id_attempted_at_idx RENAME TO payment_attempts_invoice_id_attempted_at_idx;
ALTER INDEX billing.payments_order_id_idx RENAME TO payments_invoice_id_idx;
ALTER INDEX billing.provider_bills_order_id_idx RENAME TO provider_bills_invoice_id_idx;
ALTER INDEX billing.provider_intents_order_attempt_idx RENAME TO provider_intents_invoice_attempt_idx;
ALTER INDEX billing.provider_intents_order_sale_idx RENAME TO provider_intents_invoice_sale_idx;
ALTER INDEX billing.solana_pay_receipts_order_attempt_id_idx RENAME TO solana_pay_receipts_invoice_attempt_id_idx;
ALTER INDEX billing.subscriptions_checkout_order_id_idx RENAME TO subscriptions_checkout_invoice_id_idx;

-- NOT NULL constraints are named after their table and column.
DO $$
DECLARE c record;
BEGIN
    FOR c IN SELECT conrelid::regclass AS tbl, conname FROM pg_constraint
             WHERE connamespace = 'billing'::regnamespace AND contype = 'n' AND conname LIKE '%order%'
    LOOP
        EXECUTE format('ALTER TABLE %s RENAME CONSTRAINT %I TO %I', c.tbl, c.conname, replace(c.conname, 'order', 'invoice'));
    END LOOP;
END
$$;

ALTER TRIGGER assign_checkout_order ON billing.invoices RENAME TO assign_checkout_invoice;
ALTER TRIGGER subscriptions_void_renewal_orders ON billing.subscriptions RENAME TO subscriptions_void_renewal_invoices;
ALTER FUNCTION billing.assign_checkout_order() RENAME TO assign_checkout_invoice;
ALTER FUNCTION billing.void_ended_renewal_orders() RENAME TO void_ended_renewal_invoices;

CREATE OR REPLACE FUNCTION billing.assign_checkout_invoice() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    SELECT s.checkout_invoice_id INTO NEW.checkout_invoice_id
    FROM billing.subscriptions s
    WHERE s.merchant_id = NEW.merchant_id AND s.id = NEW.subscription_id;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION billing.void_ended_renewal_invoices() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.status = 'canceled' OR NEW.deleted_at IS NOT NULL THEN
        WITH voided AS (
            UPDATE billing.invoices o
            SET status = 'void', payment_status = 'requires_payment_method', voided_at = now(), updated_at = now()
            WHERE o.merchant_id = NEW.merchant_id AND o.subscription_id = NEW.id AND o.origin = 'subscription' AND o.collection_owner = 'engine' AND o.status = 'open'
            RETURNING o.id, o.customer_id, o.total, o.currency)
        INSERT INTO billing.host_outbox (merchant_id, event_type, subject_type, subject_id, amount, currency, occurred_at, data, dedupe_key)
        SELECT NEW.merchant_id, 'invoice.voided', 'invoice', v.id, v.total, v.currency, now(),
               jsonb_build_object('customer_id', v.customer_id, 'status', 'void', 'payment_status', 'requires_payment_method'),
               'invoice.voided:' || v.id
        FROM voided v
        ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION billing.subscriptions_row_version() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    unchanged billing.subscriptions := NEW;
BEGIN
    unchanged.due_recheck_at := OLD.due_recheck_at;
    unchanged.facts_authority := OLD.facts_authority;
    unchanged.row_version := OLD.row_version;
    unchanged.destructive_run_class := OLD.destructive_run_class;
    unchanged.rail_subscription_key := OLD.rail_subscription_key;
    unchanged.checkout_invoice_id := OLD.checkout_invoice_id;
    IF billing.rail_subscription_key(NEW.rail, NEW.rail_subscription_id) IS NOT DISTINCT FROM OLD.rail_subscription_key THEN
        unchanged.rail_subscription_id := OLD.rail_subscription_id;
    END IF;
    IF unchanged IS NOT DISTINCT FROM OLD THEN
        RETURN NEW;
    END IF;
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION billing.event_subject_id(kind text, id uuid) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
    SELECT CASE WHEN id IS NULL THEN NULL ELSE
      CASE kind WHEN 'payment' THEN 'pay_' WHEN 'product' THEN 'prod_'
      WHEN 'subscription' THEN 'sub_' WHEN 'invoice' THEN 'in_'
      WHEN 'mandate' THEN 'mdt_' WHEN 'destructive_hold' THEN 'dsh_' ELSE '' END || id::text END
$$;

CREATE OR REPLACE FUNCTION billing.event_log_data(value jsonb) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
DECLARE key text; prefix text;
BEGIN
    value := COALESCE(value,'{}'::jsonb);
    -- Keep monetary integers lossless for JavaScript consumers, just like the
    -- existing typed billing API. Do not reinterpret arbitrary payload fields.
    FOREACH key IN ARRAY ARRAY['amount','total','overdue_amount','amount_floor',
      'available_amount','owed_amount','accrual_rate_per_hour','usage_rate_per_hour',
      'low_balance_amount','old_amount','new_amount','amount_due'] LOOP
        IF jsonb_typeof(value->key)='number' THEN
            value:=jsonb_set(value,ARRAY[key],to_jsonb(value->>key));
        END IF;
    END LOOP;
    FOR key,prefix IN SELECT * FROM (VALUES
      ('payment_id','pay_'),('product_id','prod_'),('subscription_id','sub_'),
      ('invoice_id','in_'),('mandate_id','mdt_'),('price_migration_id','pmig_'),
      ('from_product_id','prod_'),('to_product_id','prod_')) AS ids(key,prefix) LOOP
        IF value->>key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
            value:=jsonb_set(value,ARRAY[key],to_jsonb(prefix||(value->>key)));
        END IF;
    END LOOP;
    RETURN value;
END;
$_$;

CREATE OR REPLACE FUNCTION billing.host_event_record(h billing.host_outbox) RETURNS jsonb
LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE payload jsonb; payment record;
    source_merchant uuid:=h.merchant_id; source_payment uuid:=h.payment_id;
    account uuid; authority text;
BEGIN
    payload:=h.data || jsonb_strip_nulls(jsonb_build_object('currency',NULLIF(h.currency,''),'amount',h.amount::text));
    IF h.subject_type='customer' THEN payload:=payload||jsonb_build_object('customer_id',h.subject_id::text); END IF;
    IF h.subject_type='invoice' THEN payload:=payload||jsonb_build_object('invoice_id','in_'||h.subject_id); END IF;
    IF h.subject_type='product' THEN payload:=payload||jsonb_build_object('product_id','prod_'||h.subject_id); END IF;
    IF h.subject_type='mandate' THEN payload:=payload||jsonb_build_object('mandate_id','mdt_'||h.subject_id); END IF;
    IF h.payment_id IS NOT NULL THEN
        SELECT customer_id,subscription_id,invoice_id INTO payment
        FROM billing.payments WHERE merchant_id=source_merchant AND id=source_payment;
        payload:=payload||jsonb_strip_nulls(jsonb_build_object('payment_id','pay_'||h.payment_id,
          'customer_id',payment.customer_id,'subscription_id',payment.subscription_id,'invoice_id',payment.invoice_id));
    END IF;
    -- A host fact may already name a different account (for example the
    -- network account linked to this merchant's customer). Keep that domain
    -- identity; only fill a missing generic customer-account snapshot.
    IF payload ? 'customer_id' AND NOT payload ? 'account_id' THEN
        account:=(payload->>'customer_id')::uuid;
        SELECT COALESCE(issuer,'') INTO authority FROM billing.customers
          WHERE merchant_id=source_merchant AND id=account;
        payload:=payload||jsonb_build_object('account_id',account,'account_issuer',authority);
    END IF;
    RETURN jsonb_build_object('merchant_id',h.merchant_id,'id','hev_'||h.id,'source','host',
      'type',h.event_type,'occurred_at',h.occurred_at,'subject_type',h.subject_type,
      'subject_id',billing.event_subject_id(h.subject_type,h.subject_id),'data',billing.event_log_data(payload));
END;
$$;

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

-- Host events: invoice.* types on an invoice subject.
ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_payload_check;
UPDATE billing.host_outbox
SET event_type = 'invoice.' || substr(event_type, 7), subject_type = 'invoice',
    dedupe_key = CASE WHEN dedupe_key LIKE 'order.%' THEN 'invoice.' || substr(dedupe_key, 7) ELSE dedupe_key END
WHERE subject_type = 'order';
UPDATE billing.host_outbox SET data = (data - 'overdue_orders') || jsonb_build_object('overdue_invoices', data -> 'overdue_orders')
WHERE data ? 'overdue_orders';
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_payload_check CHECK (
    (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
    OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared', 'balance.low', 'balance.depleted') AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type = 'product.entitlements_changed' AND subject_type = 'product' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type IN ('invoice.completed', 'invoice.requires_action', 'invoice.payment_failed', 'invoice.canceled', 'invoice.expired', 'invoice.voided') AND subject_type = 'invoice' AND payment_id IS NULL AND amount IS NOT NULL AND amount >= 0 AND currency IS NOT NULL)
    OR (event_type IN ('customer.network_linked', 'customer.network_unlinked') AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type = 'mandate.revoked' AND subject_type = 'mandate' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type IN ('destructive.hold_opened', 'destructive.hold_cleared') AND subject_type = 'destructive_hold' AND payment_id IS NULL AND amount IS NULL AND currency IS NULL AND jsonb_typeof(data) = 'object')
);

-- Messages queued for a fact: invoice_* types. Notification events are
-- immutable history: readers rename an earlier fact's type and data.
UPDATE billing.customer_messages SET event_type = 'invoice_' || substr(event_type, 7)
WHERE event_type IN ('order_due', 'order_overdue', 'order_collection_stopped');

-- Ownership claims an invoice holds.
ALTER TABLE billing.ownership_claims DROP CONSTRAINT ownership_claims_holder_type_check, DROP CONSTRAINT ownership_claims_holder_check;
UPDATE billing.ownership_claims SET holder_type = 'invoice' WHERE holder_type = 'order';
ALTER TABLE billing.ownership_claims
    ADD CONSTRAINT ownership_claims_holder_type_check CHECK (holder_type IN ('invoice', 'subscription', 'product_access')),
    ADD CONSTRAINT ownership_claims_holder_check CHECK (holder_type <> 'invoice' OR holder_id = invoice_id);

UPDATE billing.reconciliation_findings SET finding_type = 'life.invoice.late_payment' WHERE finding_type = 'life.order.late_payment';

COMMENT ON TABLE billing.customer_delinquency IS 'Per-(merchant, payer, currency) arrears delinquency state: current -> grace -> delinquent, derived from overdue open usage invoices against the merchant''s declared grace window and amount floor. Evaluated when a usage invoice of the payer changes and at next_evaluation_at; a payer with an open usage invoice has a row. A projection of invoice truth; only the transition watermarks (entered_at, transition_seq) are not recomputable. Delinquency NEVER revokes an entitlement: it refuses new spend at admission and emits a host_outbox signal; the operator owns the shutoff.';
COMMENT ON COLUMN billing.customer_delinquency.next_evaluation_at IS 'The next instant time alone changes this payer''s delinquency in the currency: a usage invoice falling due or its grace window running out. Null: only a change to its usage invoices or policy does.';
COMMENT ON INDEX billing.ledger_transfers_operation_once_key IS 'The structural once-only key for EVERY transfer type. ledger.ApplyIdempotent and books.Post insert ON CONFLICT DO NOTHING against this index, so a replay is refused by the database rather than by a check-then-insert in Go. credit_grant_id, invoice_line_id and book are part of the identity (one debit per operation per FIFO lot; one posting per invoice line; usage_reversed''s leg in each book); NULLS NOT DISTINCT because the owed/payment legs carry no lot and system transfers carry no customer.';
COMMENT ON SEQUENCE billing.invoice_number_seq IS 'Source of invoices.number, one per schema, starting at 1001. A number is taken when an invoice completes; nextval never blocks and is not rolled back, so numbers may skip or arrive out of order. Unique per merchant by invoices_number_key. A restore raises it past the restored numbers.';
COMMENT ON TABLE billing.invoices IS 'One payable (in_ id): frozen lines and total in one currency, paid by at most one live invoice attempt at a time: a purchase, or a renewal of an engine-owned subscription. A decline leaves it open with last_payment_error; it is numbered from invoice_number_seq when complete. idempotency_key is the scoped Idempotency-Key of the request that created it, kept as long as the invoice. Retention: unpaid canceled and expired invoices that never started a payment attempt are deleted 397 days after they closed; every other invoice is permanent.';
COMMENT ON COLUMN billing.invoices.origin IS 'customer (the customer''s own purchase), merchant (an unpaid invoice the merchant created for them), subscription (a renewal of an engine-owned subscription) or usage (owed charges billed in arrears for a period).';
COMMENT ON COLUMN billing.invoices.status IS 'open (takes payment, or awaits the customer''s action on it), processing (the provider has its payment), complete (paid and fulfilled), canceled or expired (a purchase closed unpaid), void (a renewal its subscription''s end left unowed, or a usage invoice the merchant voided), uncollectible (a usage invoice the merchant wrote off; collecting it reopens it).';
COMMENT ON COLUMN billing.invoices.number IS 'The invoice number, taken from invoice_number_seq when the invoice completes; unique per merchant, not consecutive.';
COMMENT ON COLUMN billing.invoices.attempt_id IS 'The invoice attempt last started for the invoice.';
COMMENT ON COLUMN billing.invoices.payment_status IS 'The payment''s own status: requires_payment_method (none yet, or the last declined: last_payment_error), requires_action, processing or succeeded. A closed invoice paid late is succeeded with its payment refunded.';
COMMENT ON COLUMN billing.invoices.pay_url_secret_hash IS 'sha256 of the pay URL''s secret; the secret is never stored. Set once on a merchant invoice; on a customer''s own invoice, again each time the customer asks for its pay URL, which ends the last.';
COMMENT ON COLUMN billing.invoices.pay_url_expires_at IS 'When the pay URL stops working; no later than the invoice''s own expiry.';
COMMENT ON COLUMN billing.invoices.success_url IS 'Where a merchant invoice''s pay URL returns once paid. NULL on a customer''s own embedded pay URL, which returns nowhere: the frame tells its page.';
COMMENT ON COLUMN billing.invoices.due_at IS 'When a renewal is owed: its period''s start. An open invoice past it is overdue.';
COMMENT ON COLUMN billing.invoices.amount_paid IS 'A usage invoice''s paid part: its payments, remittances and the funded balance that repaid it. Complete once it is the total.';
COMMENT ON COLUMN billing.invoices.collection_method IS 'A usage invoice''s collection: charge_automatically (charged to the currency''s collection card) or send_invoice (the customer remits).';
COMMENT ON COLUMN billing.invoices.collection_failed_at IS 'A usage invoice''s first declined collection, which its retry schedule counts from.';
COMMENT ON COLUMN billing.invoices.next_collection_attempt_at IS 'When a usage invoice''s next scheduled charge runs: its due date when it opens, then the retry schedule''s; NULL while none is scheduled (stopped until the customer designates a card).';
COMMENT ON COLUMN billing.invoices.lines_digest IS 'sha256 of a customer invoice''s lines (prices, quantities, amounts), which POST /v1/me/invoices finds its open invoice by. NULL on a cart''s invoice and every other origin.';
COMMENT ON COLUMN billing.payments.invoice_id IS 'Required immutable bill identity for every payment and reversal. Prices and products belong to the invoice lines.';
COMMENT ON COLUMN billing.payments.ledger_transfer_id IS 'The owed payment that settled a usage invoice''s charge or remittance. A balance repayment moves no money and is only its ledger transfer.';
COMMENT ON TABLE billing.invoice_attempts IS 'An invoice payment attempt: provider execution and any customer action are tied to the invoice. Retention: attempts that expired without reaching a provider are deleted 90 days after expires_at; every other attempt is permanent.';
COMMENT ON COLUMN billing.invoice_attempts.invoice_id IS 'The invoice this payment attempt pays.';
COMMENT ON TABLE billing.invoice_lines IS 'One line of an invoice, frozen at creation: a price, its quantity (seats on a per-seat recurring price, units of a consumable, NULL on any other recurring price) and amounts, the ownership rule it was sold under, and what paying it produced (subscription_id, product_access_id). Retention: deleted with their invoice.';
COMMENT ON COLUMN billing.invoice_lines.claim_key IS 'product:<id> or tier_group:<group>: the ownership a unique line claims while its invoice is live.';
COMMENT ON TABLE billing.carts IS 'A customer''s cart: up to 100 lines ({price_id, quantity, amount}, in the order added), priced live as an invoice preview. Every change goes through one function in one transaction and sets last_changed_at; an invoice made from the cart takes its lines. Retention: carts are deleted 30 days after last_changed_at.';
COMMENT ON COLUMN billing.carts.lines IS 'JSON array of {price_id, quantity, amount}: quantity and amount as on an invoice line, absent when not given.';
COMMENT ON TABLE billing.owed_items IS 'Owed charges awaiting their usage invoice: an accrual is born pending and gates arrears exposure; finalization moves it onto an invoice (invoice_id, status billed), so it bills once. Retention: permanent, never pruned.';
COMMENT ON TABLE billing.ownership_claims IS 'At most one live ownership per customer and key (product or tier group). An invoice claims its unique lines when created; paying hands each claim to the subscription or product access it produced; cancel, expiry, the subscription''s end or the access''s revocation deletes it.';
COMMENT ON TABLE billing.solana_pay_references IS 'One Solana Pay reference per invoice attempt. pending = awaiting a transfer landed by expires_at; confirmed = one signature credited (or mirrored); expired = nothing credited by expires_at. Purchase references stay watched until watch_ends_at so a second or late transfer is recorded, then retention deletes the settled row. seen_until is the newest signature whose older history is fully processed; scan_stack holds the before-cursors of an unfinished walk down the history and scan_below the cursor whose older signatures were just processed, so no signature is ever skipped however many land on the reference; a reference is never collected mid-walk. built_transaction is the one transaction-request tx offered while its blockhash can still land. Retention: settled references are deleted after their 7-day watch window.';
COMMENT ON TABLE billing.solana_pay_receipts IS 'Every signature observed on a Solana Pay reference, recorded once. credited = the invoice attempt was paid by it (overpaid flags the excess for refund); review = money that was not credited (already_paid, late, underpaid, session_closed, wrong_asset, unreadable, settle_failed) and needs a refund or operator decision, closed by resolved_at; duplicate = the transfer already settled another reference; ignored = no value to the merchant (deleted with its reference). A transfer to one recipient in one mint is credited or reviewed at most once across every reference. Unresolved reviews refuse the billing archive. Retention: permanent for credited and review receipts; an ignored receipt goes with its settled reference.';
COMMENT ON TABLE billing.provider_bills IS 'Scoped immutable provider bill identity and accepted invoice snapshot. Multiple verified aliases can name one invoice. Settlement proof never executes collection. Retention: permanent with the financial invoice.';
COMMENT ON COLUMN billing.invoice_lines.billing_interval_months IS 'Accepted calendar-month cadence. NULL preserves historical fixed-hour and one-time lines. The initial completed invoice fixes the UTC calendar anchor.';
COMMENT ON COLUMN billing.credit_grants.funding_source IS 'purchase and recurring were paid (a one-off invoice line, or a recurring price''s cycle); grant and promotion were given; import came from another system and may declare what was paid.';
COMMENT ON COLUMN billing.ledger_transfers.credit_grant_id IS 'The credit lot the leg moves units of, or attributes money to. invoice_id and customer_id carry no foreign keys: the append-only ledger never blocks or cascades on business rows.';
COMMENT ON COLUMN billing.ledger_transfers.invoice_line_id IS 'The invoice line a general-ledger transfer belongs to. No foreign key (ledger purity).';
COMMENT ON TABLE billing.revenue_schedules IS 'One row per paid invoice line, and per currency its money was paid in, whose revenue the general ledger defers: what was funded (credited to deferred_revenue), what has been recognised, and over what service. A line priced in a merchant''s own unit is funded by its lots'' money, in each lot''s paid currency. A ratable schedule recognises by elapsed service at every midnight on the 1st in the merchant''s accounting timezone and at its end; a point-in-time one at once. Retention: permanent, never pruned.';
COMMENT ON CONSTRAINT invoices_complete_payment_check ON billing.invoices IS 'A complete invoice was paid by its payment, unless it was free, is a usage invoice, is collected by a provider, or is in a merchant''s unit: the customer''s balance paid it, in credit_spend legs naming the invoice.';
COMMENT ON TABLE billing.books_starts IS 'One row per merchant: when its general ledger booked its first receipt, by the clock that stamps payments and invoices. ledger-audit checks the postings of what was stored since; what came before waits for the books'' rebuild. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.invoices.purchase_number IS 'Set on a checkout (an invoice OpenRails started for its customer to pay): 1 + the customer''s paid purchases when it was created. Two checkouts open at once can share one. NULL on every other invoice and on checkouts created before it was recorded.';
