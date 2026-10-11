-- parent: 60 sha256:3dfcc6278d758e65d9dadbb4db218304194b67b7fa12b3c002644fa6667ceaa0
-- Repair: none-needed Each invoice becomes the order of what it billed under the same id; its payments, attempts, ledger transfers and collections name that order.
-- Usage billed in arrears is an order (origin usage), and invoices go. A
-- usage order bills the owed charges of a period (or, past the customer's
-- threshold, of the period so far): one line per accrual source, due at
-- finalization plus the customer's net terms. It carries the invoice
-- profile's document fields, its collection method and cadence, and
-- amount_paid: a funded balance and remittances pay part of it.
ALTER TABLE billing.orders
    ADD COLUMN amount_paid bigint,
    ADD COLUMN uncollectible_at timestamp with time zone,
    ADD COLUMN collection_method text,
    ADD COLUMN po_number text,
    ADD COLUMN memo text,
    ADD COLUMN tax jsonb,
    ADD COLUMN billing_contacts jsonb,
    ADD COLUMN collection_failure_count integer,
    ADD COLUMN collection_failed_at timestamp with time zone,
    ADD COLUMN next_collection_attempt_at timestamp with time zone;
ALTER TABLE billing.orders DROP CONSTRAINT orders_origin_check;
ALTER TABLE billing.orders DROP CONSTRAINT orders_status_check;
ALTER TABLE billing.orders DROP CONSTRAINT orders_renewal_check;
ALTER TABLE billing.orders DROP CONSTRAINT orders_complete_payment_check;
ALTER TABLE billing.orders
    ADD CONSTRAINT orders_origin_check CHECK (origin IN ('customer', 'merchant', 'subscription', 'usage')),
    ADD CONSTRAINT orders_status_check CHECK (status IN ('open', 'processing', 'complete', 'canceled', 'expired', 'void', 'uncollectible')),
    ADD CONSTRAINT orders_period_check CHECK (
        (origin IN ('subscription', 'usage')) = (period_starts_at IS NOT NULL AND period_ends_at IS NOT NULL AND due_at IS NOT NULL)
        AND (origin = 'subscription') = (subscription_id IS NOT NULL)
        AND (origin IN ('subscription', 'usage')) = (expires_at IS NULL)
        AND (period_ends_at IS NULL OR period_ends_at > period_starts_at)),
    ADD CONSTRAINT orders_usage_check CHECK (
        (origin = 'usage') = (amount_paid IS NOT NULL AND collection_method IS NOT NULL AND tax IS NOT NULL
                              AND billing_contacts IS NOT NULL AND collection_failure_count IS NOT NULL)
        AND (amount_paid IS NULL OR amount_paid BETWEEN 0 AND total)
        AND (status <> 'complete' OR amount_paid IS NULL OR amount_paid = total)
        AND collection_failure_count >= 0
        AND (next_collection_attempt_at IS NULL OR origin = 'usage')
        AND (collection_failed_at IS NULL OR origin = 'usage')),
    ADD CONSTRAINT orders_uncollectible_check CHECK ((status = 'uncollectible') = (uncollectible_at IS NOT NULL) AND (status <> 'uncollectible' OR origin = 'usage')),
    ADD CONSTRAINT orders_collection_method_check CHECK (collection_method IN ('charge_automatically', 'send_invoice')),
    ADD CONSTRAINT orders_po_number_check CHECK (po_number <> ''),
    ADD CONSTRAINT orders_memo_check CHECK (memo <> ''),
    ADD CONSTRAINT orders_tax_check CHECK (jsonb_typeof(tax) = 'object'),
    ADD CONSTRAINT orders_billing_contacts_check CHECK (jsonb_typeof(billing_contacts) = 'array'),
    -- A usage order a funded balance paid is complete without a payment.
    ADD CONSTRAINT orders_complete_payment_check CHECK (status <> 'complete' OR total = 0 OR payment_id IS NOT NULL OR origin = 'usage');
CREATE UNIQUE INDEX orders_usage_period_key ON billing.orders USING btree (merchant_id, customer_id, currency, period_starts_at, period_ends_at) WHERE origin = 'usage';
CREATE INDEX orders_usage_collection_idx ON billing.orders USING btree (merchant_id, next_collection_attempt_at, due_at)
    WHERE origin = 'usage' AND status = 'open' AND collection_method = 'charge_automatically';
CREATE INDEX orders_usage_open_idx ON billing.orders USING btree (merchant_id, customer_id, currency) WHERE origin = 'usage' AND status IN ('open', 'processing', 'uncollectible');
COMMENT ON COLUMN billing.orders.status IS 'open (takes payment, or awaits the customer''s action on it), processing (the provider has its payment), complete (paid and fulfilled), canceled or expired (a purchase closed unpaid), void (a renewal its subscription''s end left unowed, or a usage order the merchant voided), uncollectible (a usage order the merchant wrote off; collecting it reopens it).';
COMMENT ON COLUMN billing.orders.origin IS 'customer (the customer''s own purchase), merchant (an unpaid order the merchant created for them), subscription (a renewal of an engine-owned subscription) or usage (owed charges billed in arrears for a period).';
COMMENT ON COLUMN billing.orders.amount_paid IS 'A usage order''s paid part: its payments, remittances and the funded balance that repaid it. Complete once it is the total.';
COMMENT ON COLUMN billing.orders.collection_method IS 'A usage order''s collection: charge_automatically (charged to the currency''s collection card) or send_invoice (the customer remits).';
COMMENT ON COLUMN billing.orders.collection_failed_at IS 'A usage order''s first declined collection, which its retry schedule counts from.';
COMMENT ON COLUMN billing.orders.next_collection_attempt_at IS 'When a usage order''s next scheduled charge runs; NULL while none is scheduled.';

-- A usage line names its accrual source instead of a price.
ALTER TABLE billing.order_lines
    ADD COLUMN source text,
    ALTER COLUMN price_id DROP NOT NULL,
    ALTER COLUMN product_id DROP NOT NULL,
    ALTER COLUMN ownership DROP NOT NULL,
    DROP CONSTRAINT order_lines_quantified_check;
ALTER TABLE billing.order_lines
    ADD CONSTRAINT order_lines_quantified_check CHECK (quantity IS NOT NULL OR billing_interval_hours IS NOT NULL OR source IS NOT NULL),
    ADD CONSTRAINT order_lines_source_check CHECK ((source IS NULL) = (price_id IS NOT NULL AND product_id IS NOT NULL AND ownership IS NOT NULL) AND source <> '');
COMMENT ON COLUMN billing.order_lines.source IS 'A usage line''s accrual source (e.g. metered:<meter>); NULL on a priced line.';

-- Pending owed charges wait for their usage order.
ALTER TABLE billing.invoice_items RENAME TO owed_items;
ALTER TABLE billing.owed_items RENAME COLUMN invoice_id TO order_id;
ALTER TABLE billing.owed_items RENAME COLUMN invoice_at TO accrued_at;
ALTER TABLE billing.owed_items DROP CONSTRAINT invoice_items_customer_id_currency_invoice_id_fkey;
ALTER TABLE billing.owed_items DROP CONSTRAINT invoice_items_status_check;
UPDATE billing.owed_items SET status = 'billed' WHERE status = 'invoiced';
UPDATE billing.owed_items SET order_id = NULL WHERE order_id IS NOT NULL AND status <> 'billed';
ALTER TABLE billing.owed_items RENAME CONSTRAINT invoice_items_pkey TO owed_items_pkey;
ALTER TABLE billing.owed_items RENAME CONSTRAINT invoice_items_amount_nonneg_check TO owed_items_amount_check;
ALTER TABLE billing.owed_items RENAME CONSTRAINT invoice_items_currency_check TO owed_items_currency_check;
ALTER TABLE billing.owed_items RENAME CONSTRAINT invoice_items_customer_id_fkey TO owed_items_customer_id_fkey;
ALTER TABLE billing.owed_items RENAME CONSTRAINT invoice_items_merchant_id_fkey TO owed_items_merchant_id_fkey;
ALTER INDEX billing.invoice_items_invoice_id_idx RENAME TO owed_items_order_id_idx;
ALTER INDEX billing.invoice_items_customer_id_currency_invoice_at_idx RENAME TO owed_items_pending_idx;
ALTER INDEX billing.invoice_items_customer_id_currency_source_type_source_id_key RENAME TO owed_items_source_key;
ALTER INDEX billing.invoice_items_billed_invoice_at_idx RENAME TO owed_items_billed_idx;
ALTER TABLE billing.owed_items ADD CONSTRAINT owed_items_status_check CHECK (status IN ('pending', 'billed', 'voided') AND (order_id IS NOT NULL) = (status = 'billed'));
COMMENT ON TABLE billing.owed_items IS 'Owed charges awaiting their usage order: an accrual is born pending and gates arrears exposure; finalization moves it onto an order (order_id, status billed), so it bills once. Retention: permanent, never pruned.';
COMMENT ON TABLE billing.usage_events IS 'Append-only multi-dimensional metered usage. Source of truth for usage reporting and usage order lines. Host-priced (amount sent by the host); event + ledger debit commit in one tx. The hot admission path never reads this table. Retention: monthly partitions on occurred_at, dropped 24 months after the month''s usage was billed.';

-- Invoices become usage orders under their own ids. An invoice that billed
-- none of its own charges (a prepaid statement) was a statement only.
WITH own AS (
    SELECT merchant_id, order_id AS id, SUM(amount)::bigint AS total
    FROM billing.owed_items WHERE order_id IS NOT NULL
    GROUP BY merchant_id, order_id
)
INSERT INTO billing.orders (merchant_id, id, customer_id, origin, status, payment_status, currency, total, amount_paid, number,
    last_payment_error, period_starts_at, period_ends_at, due_at, completed_at, voided_at, uncollectible_at,
    collection_method, po_number, memo, tax, billing_contacts, collection_failure_count, collection_failed_at, next_collection_attempt_at, created_at, updated_at)
SELECT i.merchant_id, i.id, i.customer_id, 'usage',
    CASE i.status WHEN 'paid' THEN 'complete' WHEN 'voided' THEN 'void' WHEN 'uncollectible' THEN 'uncollectible' ELSE 'open' END,
    CASE i.status WHEN 'paid' THEN 'succeeded' ELSE 'requires_payment_method' END,
    i.currency, own.total,
    CASE i.status WHEN 'paid' THEN own.total ELSE GREATEST(LEAST(own.total - i.amount_due, own.total), 0) END,
    CASE i.status WHEN 'paid' THEN COALESCE(i.invoice_number, i.id::text) END,
    CASE WHEN i.last_collection_failure_code IS NOT NULL
         THEN jsonb_build_object('reason', i.last_collection_failure_code, 'message', COALESCE(i.last_collection_failure_message, i.last_collection_failure_code)) END,
    i.period_starts_at, i.period_ends_at, COALESCE(i.due_at, i.finalized_at, i.created_at),
    CASE i.status WHEN 'paid' THEN COALESCE(i.paid_at, i.updated_at) END,
    CASE i.status WHEN 'voided' THEN COALESCE(i.voided_at, i.updated_at) END,
    CASE i.status WHEN 'uncollectible' THEN COALESCE(i.uncollectible_at, i.updated_at) END,
    i.collection_method, NULLIF(i.po_number, ''), NULLIF(i.memo, ''), i.tax, i.billing_contacts, i.collection_failure_count,
    i.collection_failed_at, i.next_collection_attempt_at, i.created_at, i.updated_at
FROM billing.invoices i JOIN own ON own.merchant_id = i.merchant_id AND own.id = i.id;
UPDATE billing.owed_items it SET order_id = NULL, status = 'pending'
WHERE it.order_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM billing.orders o WHERE o.merchant_id = it.merchant_id AND o.id = it.order_id);
ALTER TABLE billing.owed_items ADD CONSTRAINT owed_items_order_id_fkey
    FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders(merchant_id, customer_id, id) ON DELETE RESTRICT;

-- One line per accrual source.
INSERT INTO billing.order_lines (merchant_id, id, order_id, customer_id, "position", description, unit_amount, amount, source, created_at)
SELECT s.merchant_id, uuidv7(), s.order_id, s.customer_id, (row_number() OVER (PARTITION BY s.merchant_id, s.order_id ORDER BY s.source) - 1)::integer,
    'Usage: ' || s.source, s.amount, s.amount, s.source, now()
FROM (
    SELECT merchant_id, order_id, customer_id, COALESCE(NULLIF(metadata ->> 'source', ''), source_type) AS source, SUM(amount)::bigint AS amount
    FROM billing.owed_items WHERE order_id IS NOT NULL
    GROUP BY merchant_id, order_id, customer_id, COALESCE(NULLIF(metadata ->> 'source', ''), source_type)
) s;

-- A collection in flight is the order's live attempt.
INSERT INTO billing.checkout_attempts (merchant_id, id, mode, rail, status, amount, currency, expires_at, rail_state, customer_id, psp_id, order_id, created_at, updated_at)
SELECT i.merchant_id, pi.id, 'order', pi.rail, 'processing', o.total - o.amount_paid, i.currency, i.period_ends_at + interval '400 days',
    jsonb_build_object('operation_id', pi.id::text), i.customer_id, pi.psp_id, i.id, now(), now()
FROM billing.invoices i
JOIN billing.orders o ON o.merchant_id = i.merchant_id AND o.id = i.id
JOIN billing.provider_intents pi ON pi.merchant_id = i.merchant_id AND pi.id = i.collection_intent_id;
UPDATE billing.orders o SET attempt_id = i.collection_intent_id, status = 'processing', payment_status = 'processing', uncollectible_at = NULL
FROM billing.invoices i
WHERE o.merchant_id = i.merchant_id AND o.id = i.id AND i.collection_intent_id IS NOT NULL;

-- Payments, attempts, owed transfers and collections name the order.
UPDATE billing.payments SET order_id = invoice_id WHERE invoice_id IS NOT NULL;
ALTER TABLE billing.payments DROP CONSTRAINT payments_payable_check;
ALTER TABLE billing.payments DROP CONSTRAINT payments_invoice_ledger_check;
ALTER TABLE billing.payments DROP CONSTRAINT payments_invoice_id_fkey;
ALTER TABLE billing.payments DROP COLUMN invoice_id;
ALTER TABLE billing.payments
    ADD CONSTRAINT payments_payable_check CHECK (num_nonnulls(order_id, price_id) = 1),
    -- An owed payment settles a usage order; a reversal settled none.
    ADD CONSTRAINT payments_ledger_transfer_check CHECK (ledger_transfer_id IS NULL OR (order_id IS NOT NULL AND refunded_payment_id IS NULL));
COMMENT ON COLUMN billing.payments.ledger_transfer_id IS 'The owed payment that settled a usage order''s charge or remittance. A balance repayment moves no money and is only its ledger transfer.';
DROP INDEX billing.payments_order_id_charge_key;
-- At most one successful charge per purchase; a usage order takes several.
CREATE UNIQUE INDEX payments_order_id_charge_key ON billing.payments USING btree (merchant_id, order_id)
    WHERE order_id IS NOT NULL AND ledger_transfer_id IS NULL AND refunded_payment_id IS NULL AND status IN ('succeeded', 'refunded') AND deleted_at IS NULL;

UPDATE billing.payment_attempts SET order_id = invoice_id WHERE invoice_id IS NOT NULL;
ALTER TABLE billing.payment_attempts DROP CONSTRAINT payment_attempts_payable_check;
ALTER TABLE billing.payment_attempts DROP CONSTRAINT payment_attempts_invoice_id_fkey;
ALTER TABLE billing.payment_attempts DROP COLUMN invoice_id;
ALTER TABLE billing.payment_attempts DROP CONSTRAINT payment_attempts_kind_check;
UPDATE billing.payment_attempts SET kind = 'collection' WHERE kind = 'invoice';
ALTER TABLE billing.payment_attempts ADD CONSTRAINT payment_attempts_kind_check
    CHECK (kind IN ('verify', 'initial', 'upgrade', 'rebill', 'dunning_retry', 'customer_retry', 'collection'));

ALTER TABLE billing.ledger_transfers RENAME COLUMN invoice_id TO order_id;
COMMENT ON COLUMN billing.ledger_transfers.grant_id IS 'Credit-lot attribution. grant_id/order_id/customer_id deliberately carry no FKs (ledger purity): the append-only ledger never blocks or cascades on business rows.';

-- An earlier collection keeps its payload (naming invoice_id) and key: its
-- receipts and ledger transfers are bound to them.
UPDATE billing.provider_intents SET intent_type = 'order_collection' WHERE intent_type = 'invoice_collection';

UPDATE billing.notifications SET event_type = CASE event_type
        WHEN 'invoice_issued' THEN 'order_due' WHEN 'invoice_overdue' THEN 'order_overdue'
        WHEN 'invoice_collection_stopped' THEN 'order_collection_stopped' END,
    data = CASE WHEN data ? 'invoice_id' THEN (data - 'invoice_id' - 'invoice_number') || jsonb_build_object('order_id', data -> 'invoice_id') ELSE data END
WHERE event_type IN ('invoice_issued', 'invoice_overdue', 'invoice_collection_stopped');

ALTER TABLE billing.customer_delinquency RENAME COLUMN overdue_invoices TO overdue_orders;
UPDATE billing.admission_denials_hourly SET denial_reason = 'delinquent_unpaid_order' WHERE denial_reason = 'delinquent_unpaid_invoice';
UPDATE billing.host_outbox SET data = (data - 'overdue_invoices') || jsonb_build_object('overdue_orders', data -> 'overdue_invoices')
WHERE data ? 'overdue_invoices';
UPDATE billing.notifications SET data = (data - 'overdue_invoices') || jsonb_build_object('overdue_orders', data -> 'overdue_invoices')
WHERE data ? 'overdue_invoices';
UPDATE billing.provider_intents
SET result_evidence = (result_evidence - 'qualified_invoice_nonexecution') || jsonb_build_object('qualified_collection_nonexecution', result_evidence -> 'qualified_invoice_nonexecution')
WHERE result_evidence ? 'qualified_invoice_nonexecution';

ALTER TABLE billing.invoice_collection_cadence RENAME TO usage_collection_cadence;
COMMENT ON TABLE billing.usage_collection_cadence IS 'Last fully scanned thirty-day usage collection period per merchant. Advancing this marker requires successful admission of the whole eligible scan; accepted operations recover independently. Included in merchant archives. Retention: permanent, never pruned.';

DROP TABLE billing.invoices;

-- Owed items and the usage cadence occupy a restore destination; invoices are gone.
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
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_meters',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'checkout_attempts',
                  'checkout_sessions',
                  'cost_observations',
                  'cost_qualifications',
                  'cost_refusals',
                  'cost_resolutions',
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customer_links',
                  'customers',
                  'dashboard_configs',
                  'destructive_run_before_images',
                  'document_sequences',
                  'failed_usage_windows',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'ledger_accounts',
                  'ledger_transfers',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'metered_rating_watermarks',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notifications',
                  'operation_authorization_extensions',
                  'operation_authorizations',
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
                  'product_archive_operations',
                  'product_entitlements',
                  'products',
                  'provider_intents',
                  'provider_mutation_logs',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'subscription_status_transitions',
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
        PERFORM billing.check_billing_restore_ledger(NEW.merchant_id);
    END IF;
    RETURN NEW;
END;
$$;

