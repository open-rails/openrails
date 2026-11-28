-- Deposits and reserves. A PSP's payout leaves its cash clearing for
-- cash_in_transit and lands in a bank account's bank_cash; what the PSP holds
-- back sits in its processor_reserve until released. Bank cash is scoped by
-- bank account, as cash clearing is by PSP, so the rollups key on both.

CREATE TABLE billing.bank_accounts (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    psp_id uuid NOT NULL,
    ref text NOT NULL CHECK (ref <> '' AND octet_length(ref) <= 256),
    name text CHECK (name <> ''),
    last4 text CHECK (last4 ~ '^[0-9A-Za-z]{1,4}$'),
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT bank_accounts_psp_id_ref_key UNIQUE (merchant_id, psp_id, ref),
    CONSTRAINT bank_accounts_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.bank_accounts IS 'A bank account, or card, a PSP pays the merchant out to, as that PSP names it: recorded by the first payout read that names it. Its bank_cash account holds what PSPs deposited there. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.bank_accounts.ref IS 'The PSP''s id for it: a Stripe external account (ba_, card_) or payout method.';
COMMENT ON COLUMN billing.bank_accounts.name IS 'The bank''s name, or a card''s brand, as the PSP reports it.';

CREATE TABLE billing.stripe_payout_reads (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    payout_id text NOT NULL CHECK (payout_id ~ '^po_[A-Za-z0-9_]{1,250}$'),
    due_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error text CHECK (last_error <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, psp_id, payout_id),
    CONSTRAINT stripe_payout_reads_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.stripe_payout_reads IS 'A Stripe payout the general ledger has yet to read to its end: queued by each notice of it, read by the billing pass''s stripe_payouts scan, deleted once it is paid, failed or canceled and its reserve lines are read.';
COMMENT ON COLUMN billing.stripe_payout_reads.due_at IS 'When it is read next: a payout not yet paid at its arrival date and each hour after; a failed read waits longer each time, and after eight failures waits (infinity) until its books.payout.unread finding is fixed. A notice reads it at once.';

CREATE INDEX stripe_payout_reads_due_at_idx ON billing.stripe_payout_reads (due_at, merchant_id, psp_id, payout_id);

-- A payout read that kept failing is parked behind its books.payout.unread
-- finding; resolving the finding reads it again.
CREATE FUNCTION billing.requeue_stripe_payout_read() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.finding_type <> 'books.payout.unread' OR NEW.status NOT IN ('fixed', 'auto_fixed')
       OR OLD.status NOT IN ('reconcile_required', 'requires_review') THEN
        RETURN NULL;
    END IF;
    UPDATE billing.stripe_payout_reads SET due_at = now(), attempts = 0, last_error = NULL
     WHERE merchant_id = NEW.merchant_id AND psp_id::text = split_part(NEW.subject_key, '/', 1)
       AND payout_id = split_part(NEW.subject_key, '/', 2) AND due_at = 'infinity';
    RETURN NULL;
END;
$$;

CREATE TRIGGER reconciliation_findings_requeue_stripe_payout_read AFTER UPDATE OF status ON billing.reconciliation_findings
    FOR EACH ROW EXECUTE FUNCTION billing.requeue_stripe_payout_read();

-- The chart: bank cash per bank account; a payout in transit and a reserve
-- held, per PSP. A payout never lands or returns more than was sent, so cash
-- in transit never goes below zero.
ALTER TABLE billing.ledger_accounts ADD COLUMN bank_account_id uuid;
ALTER TABLE billing.ledger_accounts ADD CONSTRAINT ledger_accounts_bank_account_id_fkey
    FOREIGN KEY (merchant_id, bank_account_id) REFERENCES billing.bank_accounts (merchant_id, id);
COMMENT ON COLUMN billing.ledger_accounts.psp_id IS 'The PSP of a cash_clearing, cash_in_transit or processor_reserve account, one per PSP and currency; NULL for every other account.';
COMMENT ON COLUMN billing.ledger_accounts.bank_account_id IS 'The bank account of a bank_cash account, one per bank account and currency; NULL for every other account.';
ALTER TABLE billing.ledger_accounts DROP CONSTRAINT ledger_accounts_type_check;
ALTER TABLE billing.ledger_accounts ADD CONSTRAINT ledger_accounts_type_check CHECK (
    book = 'units' AND class = 'held' AND account_type = 'customer_units'
    OR book = 'units' AND class = 'owed' AND account_type = 'customer_owed'
    OR book = 'units' AND class = 'issued' AND account_type IN ('units_sold', 'units_granted')
    OR book = 'units' AND class = 'retired' AND account_type IN ('units_redeemed', 'units_expired', 'units_revoked', 'units_refunded')
    OR book = 'general' AND class = 'asset' AND account_type IN ('cash_clearing', 'cash_in_transit', 'processor_reserve', 'bank_cash', 'external_receipts', 'usage_receivable')
    OR book = 'general' AND class = 'contra_asset' AND account_type = 'allowance_doubtful'
    OR book = 'general' AND class = 'liability' AND account_type IN ('unapplied_cash', 'deferred_revenue', 'deferred_revenue_credit', 'tax_payable')
    OR book = 'general' AND class = 'revenue' AND account_type IN ('subscription_revenue', 'one_time_revenue', 'usage_revenue', 'breakage_revenue')
    OR book = 'general' AND class = 'contra_revenue' AND account_type IN ('refunds', 'chargebacks', 'revenue_adjustments', 'promotional_credit')
    OR book = 'general' AND class = 'expense' AND account_type IN ('processor_fees', 'chargeback_fees', 'bad_debt')
    OR book = 'general' AND class = 'equity' AND account_type = 'opening_balance_equity');
ALTER TABLE billing.ledger_accounts DROP CONSTRAINT ledger_accounts_scope_check;
ALTER TABLE billing.ledger_accounts ADD CONSTRAINT ledger_accounts_scope_check CHECK (CASE
    WHEN account_type IN ('customer_units', 'customer_owed', 'usage_receivable', 'deferred_revenue_credit') THEN customer_id IS NOT NULL AND psp_id IS NULL AND bank_account_id IS NULL
    WHEN account_type IN ('cash_clearing', 'cash_in_transit', 'processor_reserve') THEN customer_id IS NULL AND psp_id IS NOT NULL AND bank_account_id IS NULL
    WHEN account_type = 'bank_cash' THEN customer_id IS NULL AND psp_id IS NULL AND bank_account_id IS NOT NULL
    ELSE customer_id IS NULL AND psp_id IS NULL AND bank_account_id IS NULL
END);
ALTER TABLE billing.ledger_accounts DROP CONSTRAINT ledger_accounts_general_sign_check;
ALTER TABLE billing.ledger_accounts ADD CONSTRAINT ledger_accounts_general_sign_check CHECK (book = 'units'
    OR debits_must_not_exceed_credits = (account_type IN ('allowance_doubtful', 'unapplied_cash', 'deferred_revenue', 'deferred_revenue_credit'))
    AND credits_must_not_exceed_debits = (account_type IN ('usage_receivable', 'cash_in_transit')));
DROP INDEX billing.ledger_accounts_account_type_currency_psp_id_key;
CREATE UNIQUE INDEX ledger_accounts_account_type_currency_psp_id_key ON billing.ledger_accounts (merchant_id, account_type, currency, psp_id, bank_account_id) NULLS NOT DISTINCT
    WHERE customer_id IS NULL;

ALTER TABLE billing.ledger_transfers DROP CONSTRAINT ledger_transfers_type_check;
ALTER TABLE billing.ledger_transfers ADD CONSTRAINT ledger_transfers_type_check CHECK (
    book = 'units' AND transfer_type IN ('deposit', 'credit_spend', 'credit_expire', 'credit_revoke', 'credit_reinstate', 'owed_accrual', 'owed_payment', 'owed_writeoff', 'owed_repayment', 'owed_reopened', 'credit_refund', 'credit_refund_restore', 'credit_refund_funding', 'usage_reversed')
    OR book = 'general' AND transfer_type IN ('cash_received', 'cash_applied', 'cash_returned', 'revenue_adjusted', 'revenue_recognized', 'refund_deferred', 'refund_recognized', 'chargeback_deferred', 'chargeback_recognized', 'dispute_won', 'credit_released', 'credit_breakage', 'credit_forfeited', 'credit_forfeit_reversed', 'promotional_credit_applied', 'usage_accrued', 'usage_collected', 'usage_repaid', 'usage_waived', 'usage_reopened', 'bad_debt', 'bad_debt_recovery', 'processor_fee', 'chargeback_fee', 'payout_sent', 'cash_deposited', 'reserve_held', 'reserve_released', 'opening_balance', 'usage_reversed'));

-- The rollups key on the bank account beside the PSP.
ALTER TABLE billing.ledger_type_totals ADD COLUMN bank_account_id uuid;
ALTER TABLE billing.ledger_type_totals DROP CONSTRAINT ledger_type_totals_account_type_key;
ALTER TABLE billing.ledger_type_totals ADD CONSTRAINT ledger_type_totals_account_type_key
    UNIQUE NULLS NOT DISTINCT (merchant_id, book, currency, account_type, psp_id, bank_account_id);
COMMENT ON TABLE billing.ledger_type_totals IS 'What every account of one type was credited and debited, summed: one row per merchant, book, currency, account type and its PSP (cash_clearing, cash_in_transit, processor_reserve) or bank account (bank_cash). It is a merchant-wide general-ledger account''s balance, since that account keeps no counters; settling it raises books.sign_breach when it crosses that account''s sign rule, and never refuses the commit. Kept at commit for the transfers a transaction posted; ledger-audit and the billing restore check recompute it from ledger_transfers.';

ALTER TABLE billing.ledger_daily ADD COLUMN bank_account_id uuid;
ALTER TABLE billing.ledger_daily DROP CONSTRAINT ledger_daily_bucket_key;
ALTER TABLE billing.ledger_daily ADD CONSTRAINT ledger_daily_bucket_key
    UNIQUE NULLS NOT DISTINCT (merchant_id, book, currency, account_type, bucket_at, transfer_type, psp_id, bank_account_id, product_id);
COMMENT ON TABLE billing.ledger_daily IS 'What the transfers of one type credited and debited the accounts of one type in one 15-minute UTC bucket of effective_at: per merchant, book, currency, account type, bucket, transfer type, the account''s PSP or bank account, and product. A reader groups buckets into days in the accounting timezone; a value at a past instant is the total less the flows after it. Kept at commit for the transfers a transaction posted; ledger-audit and the billing restore check recompute it from ledger_transfers. Retention: permanent, never pruned.';

CREATE OR REPLACE FUNCTION billing.settle_ledger_rollups(p_merchant uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    INSERT INTO billing.ledger_type_totals AS r (merchant_id, book, currency, account_type, psp_id, bank_account_id, credits_posted, debits_posted)
    SELECT p_merchant, t.book, t.currency, a.account_type, a.psp_id, a.bank_account_id,
           sum(CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE 0 END)::bigint,
           sum(CASE WHEN a.id = t.debit_account_id THEN t.amount ELSE 0 END)::bigint
      FROM billing.ledger_rollup_pending p
      JOIN billing.ledger_transfers t ON t.merchant_id = p.merchant_id AND t.id = p.transfer_id
      JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
     WHERE p.merchant_id = p_merchant
     GROUP BY 2, 3, 4, 5, 6
     ORDER BY 2, 3, 4, 5, 6
    ON CONFLICT (merchant_id, book, currency, account_type, psp_id, bank_account_id) DO UPDATE
    SET credits_posted = r.credits_posted + EXCLUDED.credits_posted, debits_posted = r.debits_posted + EXCLUDED.debits_posted;
    PERFORM billing.record_sign_breach(p_merchant, t.account_type, t.currency, NULL, t.credits_posted, t.debits_posted)
       FROM billing.ledger_type_totals t
       JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.book = t.book AND a.customer_id IS NULL
        AND a.account_type = t.account_type AND a.currency = t.currency AND a.psp_id IS NOT DISTINCT FROM t.psp_id
        AND a.bank_account_id IS NOT DISTINCT FROM t.bank_account_id
      WHERE t.merchant_id = p_merchant AND t.book = 'general'
        AND (a.debits_must_not_exceed_credits AND t.debits_posted > t.credits_posted
             OR a.credits_must_not_exceed_debits AND t.credits_posted > t.debits_posted)
        AND EXISTS (SELECT 1 FROM billing.ledger_rollup_pending p
                      JOIN billing.ledger_transfers x ON x.merchant_id = p.merchant_id AND x.id = p.transfer_id
                     WHERE p.merchant_id = p_merchant AND a.id IN (x.debit_account_id, x.credit_account_id))
      ORDER BY t.account_type, t.currency;
    INSERT INTO billing.ledger_daily AS r (merchant_id, book, currency, account_type, bucket_at, transfer_type, psp_id, bank_account_id, product_id, credits, debits)
    SELECT p_merchant, t.book, t.currency, a.account_type, date_bin('15 minutes', t.effective_at, TIMESTAMPTZ '2000-01-01 00:00:00+00'),
           t.transfer_type, a.psp_id, a.bank_account_id, t.product_id,
           sum(CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE 0 END)::bigint,
           sum(CASE WHEN a.id = t.debit_account_id THEN t.amount ELSE 0 END)::bigint
      FROM billing.ledger_rollup_pending p
      JOIN billing.ledger_transfers t ON t.merchant_id = p.merchant_id AND t.id = p.transfer_id
      JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
     WHERE p.merchant_id = p_merchant
     GROUP BY 2, 3, 4, 5, 6, 7, 8, 9
     ORDER BY 2, 3, 4, 5, 6, 7, 8, 9
    ON CONFLICT (merchant_id, book, currency, account_type, bucket_at, transfer_type, psp_id, bank_account_id, product_id) DO UPDATE
    SET credits = r.credits + EXCLUDED.credits, debits = r.debits + EXCLUDED.debits;
    DELETE FROM billing.ledger_rollup_pending WHERE merchant_id = p_merchant;
END;
$$;

CREATE OR REPLACE FUNCTION billing.check_billing_restore_ledger(p_merchant uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF p_merchant IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    IF EXISTS (SELECT 1 FROM billing.ledger_accounts a
        LEFT JOIN (SELECT credit_account_id id,sum(amount) amount FROM billing.ledger_transfers WHERE merchant_id=p_merchant GROUP BY credit_account_id) c ON c.id=a.id AND (a.book='units' OR a.customer_id IS NOT NULL)
        LEFT JOIN (SELECT debit_account_id id,sum(amount) amount FROM billing.ledger_transfers WHERE merchant_id=p_merchant GROUP BY debit_account_id) d ON d.id=a.id AND (a.book='units' OR a.customer_id IS NOT NULL)
        WHERE a.merchant_id=p_merchant AND (a.credits_posted<>coalesce(c.amount,0) OR a.debits_posted<>coalesce(d.amount,0)))
       OR EXISTS (SELECT a.book, a.currency FROM billing.ledger_accounts a
           LEFT JOIN billing.ledger_type_totals t ON a.book='general' AND a.customer_id IS NULL AND t.merchant_id=a.merchant_id AND t.book=a.book
                AND t.currency=a.currency AND t.account_type=a.account_type AND t.psp_id IS NOT DISTINCT FROM a.psp_id
                AND t.bank_account_id IS NOT DISTINCT FROM a.bank_account_id
           WHERE a.merchant_id=p_merchant GROUP BY a.book, a.currency
           HAVING sum(CASE WHEN a.book='general' AND a.customer_id IS NULL THEN coalesce(t.credits_posted::numeric-t.debits_posted::numeric,0)
                           ELSE a.credits_posted::numeric-a.debits_posted::numeric END)<>0)
       OR EXISTS (SELECT 1 FROM billing.ledger_transfers t
           JOIN billing.ledger_accounts d ON d.merchant_id=t.merchant_id AND d.id=t.debit_account_id
           JOIN billing.ledger_accounts c ON c.merchant_id=t.merchant_id AND c.id=t.credit_account_id WHERE t.merchant_id=p_merchant
           AND (d.currency<>t.currency OR c.currency<>t.currency OR d.book<>t.book OR c.book<>t.book
                OR (d.customer_id IS NOT NULL AND d.customer_id IS DISTINCT FROM t.customer_id)
                OR (c.customer_id IS NOT NULL AND c.customer_id IS DISTINCT FROM t.customer_id)))
       OR EXISTS (WITH legs AS (SELECT t.book, t.currency, a.account_type, a.psp_id, a.bank_account_id,
                CASE WHEN a.id=t.credit_account_id THEN t.amount ELSE 0 END AS credit,
                CASE WHEN a.id=t.debit_account_id THEN t.amount ELSE 0 END AS debit
                FROM billing.ledger_transfers t JOIN billing.ledger_accounts a ON a.merchant_id=t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
                WHERE t.merchant_id=p_merchant),
            logged AS (SELECT book, currency, account_type, psp_id, bank_account_id, sum(credit)::bigint AS credits, sum(debit)::bigint AS debits FROM legs GROUP BY 1, 2, 3, 4, 5),
            kept AS (SELECT book, currency, account_type, psp_id, bank_account_id, credits_posted, debits_posted FROM billing.ledger_type_totals WHERE merchant_id=p_merchant)
           (SELECT * FROM logged EXCEPT SELECT * FROM kept) UNION ALL (SELECT * FROM kept EXCEPT SELECT * FROM logged))
       OR EXISTS (WITH legs AS (SELECT t.book, t.currency, a.account_type,
                date_bin('15 minutes', t.effective_at, TIMESTAMPTZ '2000-01-01 00:00:00+00') AS bucket_at, t.transfer_type, a.psp_id, a.bank_account_id, t.product_id,
                CASE WHEN a.id=t.credit_account_id THEN t.amount ELSE 0 END AS credit,
                CASE WHEN a.id=t.debit_account_id THEN t.amount ELSE 0 END AS debit
                FROM billing.ledger_transfers t JOIN billing.ledger_accounts a ON a.merchant_id=t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
                WHERE t.merchant_id=p_merchant),
            logged AS (SELECT book, currency, account_type, bucket_at, transfer_type, psp_id, bank_account_id, product_id, sum(credit)::bigint AS credits, sum(debit)::bigint AS debits FROM legs GROUP BY 1, 2, 3, 4, 5, 6, 7, 8),
            kept AS (SELECT book, currency, account_type, bucket_at, transfer_type, psp_id, bank_account_id, product_id, credits, debits FROM billing.ledger_daily WHERE merchant_id=p_merchant)
           (SELECT * FROM logged EXCEPT SELECT * FROM kept) UNION ALL (SELECT * FROM kept EXCEPT SELECT * FROM logged)) THEN
        RAISE EXCEPTION 'billing restore ledger integrity mismatch' USING ERRCODE='23514';
    END IF;
END;
$$;

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
                  'bank_accounts',
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
                  'mrr_movement_lines',
                  'mrr_movements',
                  'mrr_risk_cases',
                  'mrr_risk_episodes',
                  'nmi_bulk_checkpoints',
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
                  'stripe_payout_reads',
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
