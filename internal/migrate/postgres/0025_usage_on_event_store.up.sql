-- Usage lives in the event store (ClickHouse): its raw events and the
-- rollups that limits, balances and history read. PostgreSQL keeps what the
-- ledger took of it (usage_postings), who has usage it has not taken
-- (usage_unposted) and the rate cards it was priced under
-- (usage_card_prints). A database whose usage accruals the ledger has not
-- taken in full is refused: post them on the build that wrote them first.
DO $$
DECLARE pending record;
BEGIN
    SELECT merchant_id, count(*) AS accruals INTO pending
    FROM billing.usage_accruals WHERE amount > posted_amount
    GROUP BY merchant_id ORDER BY merchant_id LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'merchant % has % usage accruals the ledger has not taken: post them on the previous build (its billing pass, or any balance movement) and migrate again', pending.merchant_id, pending.accruals
            USING ERRCODE = '55000';
    END IF;
END $$;

DROP TABLE billing.usage_accruals;
DROP TABLE billing.usage_daily;
DROP TABLE billing.usage_minutes;
DROP TABLE billing.meter_period_quantities;
DROP TABLE billing.failed_usage_windows;

-- A window row is its identity, capacity, adjustments and when it first
-- reached capacity; what usage took of it is the event store's. A rate
-- limit's level and a rolling limit's buckets are read there too: only a
-- bucket a staff adjustment or a reached capacity names keeps a row.
DELETE FROM billing.limit_windows w USING billing.catalog_limits l
WHERE l.merchant_id = w.merchant_id AND l.key = w.limit_key
  AND (l.reset_kind = 'rate' OR (l.reset_kind = 'rolling' AND w.adjusted = 0 AND w.reached_at IS NULL));
ALTER TABLE billing.limit_windows DROP COLUMN settled;
COMMENT ON TABLE billing.limit_windows IS 'One customer''s window of one limit, identified by the server clock: a session or first-use window a use opened, a window staff adjusted, or one whose usage reached capacity; a rolling limit''s bucket, a rate or lifetime limit''s one row, or a billing policy''s spend window (spend:<identity>). What usage took of it is in the event store (limit_usage). Retention: rows are deleted 61 days after window_ends_at; a row without an end stays while its customer does.';
COMMENT ON COLUMN billing.limit_windows.reached_at IS 'When a read first found usage and holds at the window''s capacity; a rate or lifetime limit''s one row keeps the first time.';

CREATE TABLE billing.usage_card_prints (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    print text NOT NULL CHECK (print ~ '^[0-9a-f]{16}$'),
    card jsonb NOT NULL CHECK (jsonb_typeof(card) = 'object'),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, print)
);
COMMENT ON TABLE billing.usage_card_prints IS 'A rate card''s terms as usage was priced under them, by print (a hash of the terms), written when usage first names it: the event store''s charges name the print, and a period''s price is taken card by card in the order the prints were first written. Retention: permanent, never pruned.';

CREATE TABLE billing.usage_unposted (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    since_at timestamptz NOT NULL,
    due_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, customer_id, currency),
    CONSTRAINT usage_unposted_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT usage_unposted_due_at_check CHECK (due_at > since_at)
);
COMMENT ON TABLE billing.usage_unposted IS 'A customer whose usage in a currency the ledger has not taken in full: the event store holds it, its balance reads it there, and the month-end close or the next movement of the balance posts it. Intake writes the row in the transaction that commits the usage, and a posting locks it; a posting that leaves nothing to post deletes it.';
COMMENT ON COLUMN billing.usage_unposted.since_at IS 'The start of the oldest pricing period whose usage may not be posted in full: a posting reads from it.';
COMMENT ON COLUMN billing.usage_unposted.due_at IS 'When the close posts it: the end of the month its oldest unposted usage occurred in, or when a period whose price fell may be credited.';

CREATE INDEX usage_unposted_due_at_idx ON billing.usage_unposted (due_at, merchant_id, customer_id, currency);

CREATE TABLE billing.usage_postings (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    period_starts_at timestamptz NOT NULL,
    period_ends_at timestamptz NOT NULL,
    cut_at timestamptz NOT NULL,
    reason text NOT NULL CHECK (reason IN ('close', 'movement', 'reversal')),
    amount bigint NOT NULL CHECK (amount <> 0),
    lines jsonb NOT NULL CHECK (jsonb_typeof(lines) = 'array' AND octet_length(lines::text) <= 262144),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT usage_postings_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT usage_postings_period_ends_at_check CHECK (period_ends_at > period_starts_at),
    CONSTRAINT usage_postings_amount_check CHECK ((reason = 'reversal') = (amount < 0))
);
COMMENT ON TABLE billing.usage_postings IS 'One posting of a customer''s usage in one pricing period to the ledger: the price of the period''s deduplicated usage before cut_at, less what its earlier postings took. A posting draws the balance first, one credit_spend per lot, and leaves the rest owed in one owed_accrual, at (usage_posting, usage_posting:<id>, 1); a reversal credits a period whose price fell, as usage_reversed. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.usage_postings.cut_at IS 'Usage that occurred before it is in the posting: the month''s end in the merchant''s accounting zone, or the balance movement the posting preceded.';
COMMENT ON COLUMN billing.usage_postings.reason IS 'close: the month-end close; movement: before a movement of the balance; reversal: a period whose price fell below what posted, once its late usage window closed.';
COMMENT ON COLUMN billing.usage_postings.amount IS 'What the posting moved: positive for usage, negative for a reversal.';
COMMENT ON COLUMN billing.usage_postings.lines IS 'Per meter, cell and card print, what the posting added: [{meter_key, cell, print, requests, quantity, stated, amount}].';

CREATE INDEX usage_postings_period_idx ON billing.usage_postings (merchant_id, customer_id, currency, period_starts_at);

-- A volume tier whose price fell is credited back.
ALTER TABLE billing.ledger_transfers DROP CONSTRAINT ledger_transfers_type_check;
ALTER TABLE billing.ledger_transfers ADD CONSTRAINT ledger_transfers_type_check CHECK (transfer_type IN ('deposit', 'deposit_bonus', 'credit_purchase_revenue', 'credit_spend', 'credit_expire', 'credit_revoke', 'credit_reinstate', 'owed_accrual', 'owed_payment', 'owed_writeoff', 'owed_repayment', 'credit_refund', 'credit_refund_restore', 'credit_refund_cash', 'credit_refund_cash_restore', 'credit_refund_funding', 'usage_reversed'));

-- An operator's discard of usage the event store no longer holds is recorded.
ALTER TABLE billing.maintenance_runs DROP CONSTRAINT maintenance_runs_kind_check;
ALTER TABLE billing.maintenance_runs ADD CONSTRAINT maintenance_runs_kind_check CHECK ((
        (kind IN ('prune', 'approval', 'merchant_purge', 'usage_discard') AND btrim(actor) <> ''
         AND summary IS NULL AND inventory_manifest IS NULL AND inventory_total_rows IS NULL)
        OR (kind = 'billing_restore' AND actor = 'merchantarchive' AND status IN ('running', 'completed')
         AND psp_id IS NULL AND coverage IS NULL AND expected_rows IS NULL AND affected IS NULL
         AND reversed_at IS NULL AND reversed_by IS NULL AND note IS NULL
         AND inventory_manifest IS NULL AND inventory_total_rows IS NULL
         AND ((status = 'running' AND finished_at IS NULL AND summary IS NULL)
              OR (status = 'completed' AND finished_at IS NOT NULL AND jsonb_typeof(summary) = 'object'
                  AND summary ?& ARRAY['digest', 'rows'] AND jsonb_typeof(summary -> 'digest') = 'string'
                  AND jsonb_typeof(summary -> 'rows') = 'number' AND (summary ->> 'digest') ~ '^[0-9a-f]{64}$' AND (summary ->> 'rows')::bigint >= 0)))
        OR (kind = 'purge_inventory' AND status = 'completed' AND finished_at IS NOT NULL AND psp_id IS NULL
         AND coverage IS NULL AND expected_rows IS NULL AND affected IS NULL
         AND reversed_at IS NULL AND reversed_by IS NULL AND summary IS NULL
         AND inventory_manifest IS NOT NULL AND jsonb_typeof(inventory_manifest) = 'object'
         AND jsonb_typeof(inventory_manifest -> 'total_rows') = 'number'
         AND inventory_total_rows IS NOT NULL AND inventory_total_rows >= 0
         AND inventory_total_rows = (inventory_manifest ->> 'total_rows')::bigint)
    ) IS TRUE);

-- The restore guard names the usage tables.
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
                  'ledger_transfers',
                  'limit_adjustments',
                  'limit_hold_parts',
                  'limit_holds',
                  'limit_windows',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'money_settings',
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
                  'subscription_status_transitions',
                  'subscription_billing_changes',
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
