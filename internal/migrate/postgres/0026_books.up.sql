-- Books: the ledger's accounts and transfers become the units book, renamed
-- in place, beside an empty general ledger in the same tables. Every account
-- and transfer belongs to one book; the general ledger holds money only.

-- In the order a running writer takes them: a transfer, then its accounts.
LOCK TABLE billing.ledger_transfers, billing.ledger_accounts IN ACCESS EXCLUSIVE MODE;

-- Accounts carry their book, class and, for cash clearing, their PSP. The
-- units book's accounts are renamed; no transfer moves.
ALTER TABLE billing.ledger_accounts
    ADD COLUMN book text,
    ADD COLUMN class text,
    ADD COLUMN psp_id uuid,
    DROP CONSTRAINT ledger_accounts_type_check;
ALTER TABLE billing.ledger_accounts DISABLE TRIGGER guard_ledger_account_facts;
UPDATE billing.ledger_accounts SET
    book = 'units',
    account_type = CASE account_type
        WHEN 'customer_balance' THEN 'customer_units'
        WHEN 'arrears_liability' THEN 'customer_owed'
        WHEN 'processor_clearing' THEN 'units_sold'
        WHEN 'promotional_funding' THEN 'units_granted'
        WHEN 'platform_revenue' THEN 'units_redeemed'
        WHEN 'expired_credits' THEN 'units_expired'
        WHEN 'revoked_credits' THEN 'units_revoked'
        WHEN 'credit_refund_clearing' THEN 'units_refund_clearing'
        WHEN 'credit_refund_loss' THEN 'units_refund_loss'
    END,
    class = CASE account_type
        WHEN 'customer_balance' THEN 'held'
        WHEN 'arrears_liability' THEN 'owed'
        WHEN 'processor_clearing' THEN 'issued'
        WHEN 'promotional_funding' THEN 'issued'
        WHEN 'credit_refund_clearing' THEN 'clearing'
        WHEN 'credit_refund_loss' THEN 'clearing'
        ELSE 'retired'
    END;
ALTER TABLE billing.ledger_accounts ENABLE TRIGGER guard_ledger_account_facts;
ALTER TABLE billing.ledger_accounts
    ALTER COLUMN book SET NOT NULL,
    ALTER COLUMN class SET NOT NULL,
    ADD CONSTRAINT ledger_accounts_type_check CHECK (
        book = 'units' AND class = 'held' AND account_type = 'customer_units'
        OR book = 'units' AND class = 'owed' AND account_type = 'customer_owed'
        OR book = 'units' AND class = 'issued' AND account_type IN ('units_sold', 'units_granted')
        OR book = 'units' AND class = 'retired' AND account_type IN ('units_redeemed', 'units_expired', 'units_revoked', 'units_refunded')
        OR book = 'units' AND class = 'clearing' AND account_type IN ('units_refund_clearing', 'units_refund_loss')
        OR book = 'general' AND class = 'asset' AND account_type IN ('cash_clearing', 'external_receipts', 'usage_receivable')
        OR book = 'general' AND class = 'contra_asset' AND account_type = 'allowance_doubtful'
        OR book = 'general' AND class = 'liability' AND account_type IN ('unapplied_cash', 'deferred_revenue', 'deferred_revenue_credit', 'tax_payable')
        OR book = 'general' AND class = 'revenue' AND account_type IN ('subscription_revenue', 'one_time_revenue', 'usage_revenue', 'breakage_revenue')
        OR book = 'general' AND class = 'contra_revenue' AND account_type IN ('refunds', 'chargebacks', 'revenue_adjustments', 'promotional_credit')
        OR book = 'general' AND class = 'expense' AND account_type IN ('processor_fees', 'chargeback_fees', 'bad_debt')
        OR book = 'general' AND class = 'equity' AND account_type = 'opening_balance_equity'),
    ADD CONSTRAINT ledger_accounts_scope_check CHECK (CASE
        WHEN account_type IN ('customer_units', 'customer_owed', 'usage_receivable', 'deferred_revenue_credit') THEN customer_id IS NOT NULL AND psp_id IS NULL
        WHEN account_type = 'cash_clearing' THEN customer_id IS NULL AND psp_id IS NOT NULL
        ELSE customer_id IS NULL AND psp_id IS NULL
    END),
    ADD CONSTRAINT ledger_accounts_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id),
    -- The general ledger's sign rules are the chart's, and it holds only the
    -- registry's currencies (internal/currency), never a merchant's own unit.
    ADD CONSTRAINT ledger_accounts_general_sign_check CHECK (book = 'units'
        OR debits_must_not_exceed_credits = (account_type IN ('allowance_doubtful', 'unapplied_cash', 'deferred_revenue', 'deferred_revenue_credit'))
        AND credits_must_not_exceed_debits = (account_type = 'usage_receivable')),
    ADD CONSTRAINT ledger_accounts_general_currency_check CHECK (book = 'units'
        OR currency IN ('AED', 'AUD', 'BRL', 'CAD', 'CHF', 'CNY', 'CZK', 'DKK', 'EUR', 'GBP', 'HKD', 'ILS', 'INR', 'JPY', 'KRW', 'MXN', 'MYR', 'NOK', 'NZD', 'PHP', 'PLN', 'PYUSD', 'SAR', 'SEK', 'SGD', 'SOL', 'THB', 'TRY', 'USD', 'USD1', 'USDC', 'USDG', 'USDT', 'ZAR'));
DROP INDEX billing.ledger_accounts_account_type_currency_key;
CREATE UNIQUE INDEX ledger_accounts_account_type_currency_psp_id_key ON billing.ledger_accounts (merchant_id, account_type, currency, psp_id) NULLS NOT DISTINCT
    WHERE customer_id IS NULL;
COMMENT ON TABLE billing.ledger_accounts IS 'Double-entry accounts of two books: the units book (a customer''s units held and owed, at face) and the general ledger (money, registry currencies only). One account belongs to one (merchant, book, currency); its counters are maintained from immutable ledger_transfers, except a merchant-wide general-ledger account''s, which stay 0: its balance is its ledger_type_totals row. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.ledger_accounts.customer_id IS 'Set for customer-scoped accounts (customer_units, customer_owed, usage_receivable, deferred_revenue_credit); NULL for system accounts, one per merchant, currency and PSP.';
COMMENT ON COLUMN billing.ledger_accounts.account_type IS 'The account''s role in its book (books.Chart). customer_owed is per customer: its negated balance is what the customer owes, read O(1) on the admission path.';
COMMENT ON COLUMN billing.ledger_accounts.book IS 'units or general. A transfer never crosses books.';
COMMENT ON COLUMN billing.ledger_accounts.class IS 'The account type''s class, which sets its normal side: units book held, owed, issued, retired or clearing; general ledger asset, contra_asset, liability, revenue, contra_revenue, expense or equity.';
COMMENT ON COLUMN billing.ledger_accounts.psp_id IS 'The PSP of a cash_clearing account, one per PSP and currency; NULL for every other account.';
COMMENT ON COLUMN billing.ledger_accounts.debits_must_not_exceed_credits IS 'Sign rule: the balance (credits - debits) may not go below zero. Set by the chart. The units book refuses a breach, less a floor the transfer supplies; the general ledger never refuses one: it raises books.sign_breach, a merchant-wide account''s on its type total at commit.';

-- Transfers carry their book, the instant they take effect and the order line
-- and product they belong to. Existing transfers are the units book's,
-- effective when they posted.
ALTER TABLE billing.ledger_transfers
    ADD COLUMN book text,
    ADD COLUMN effective_at timestamptz,
    ADD COLUMN order_line_id uuid,
    ADD COLUMN product_id uuid;
ALTER TABLE billing.ledger_transfers DISABLE TRIGGER immutable_ledger_transfers;
UPDATE billing.ledger_transfers SET book = 'units', effective_at = created_at;
ALTER TABLE billing.ledger_transfers ENABLE TRIGGER immutable_ledger_transfers;
ALTER TABLE billing.ledger_transfers DROP CONSTRAINT ledger_transfers_type_check;
ALTER TABLE billing.ledger_transfers
    ALTER COLUMN book SET NOT NULL,
    ALTER COLUMN effective_at SET NOT NULL,
    -- A general-ledger transfer carries no floor: its sign rules never refuse.
    ADD CONSTRAINT ledger_transfers_general_floor_check CHECK (book = 'units' OR allow_debit_negative_up_to = 0),
    ADD CONSTRAINT ledger_transfers_type_check CHECK (
        book = 'units' AND transfer_type IN ('deposit', 'deposit_bonus', 'credit_purchase_revenue', 'credit_spend', 'credit_expire', 'credit_revoke', 'credit_reinstate', 'owed_accrual', 'owed_payment', 'owed_writeoff', 'owed_repayment', 'credit_refund', 'credit_refund_restore', 'credit_refund_cash', 'credit_refund_cash_restore', 'credit_refund_funding', 'usage_reversed')
        OR book = 'general' AND transfer_type IN ('cash_received', 'cash_applied', 'cash_returned', 'revenue_adjusted', 'revenue_recognized', 'refund_deferred', 'refund_recognized', 'chargeback_deferred', 'chargeback_recognized', 'dispute_won', 'credit_released', 'credit_breakage', 'credit_forfeited', 'credit_forfeit_reversed', 'promotional_credit_applied', 'usage_accrued', 'usage_collected', 'usage_repaid', 'usage_waived', 'usage_reopened', 'bad_debt', 'bad_debt_recovery', 'processor_fee', 'chargeback_fee', 'opening_balance', 'usage_reversed'));
COMMENT ON TABLE billing.ledger_transfers IS 'Immutable double-entry transfers. Append-only. A transfer moves amount debit->credit within ONE (merchant, book, currency) ledger; capture/void/refund/expiry are NEW rows, never updates. ledger_accounts counters and the ledger_daily and ledger_type_totals rollups are maintained projections of this table. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.ledger_transfers.book IS 'The book of both accounts: units or general. transfer_type belongs to it; usage_reversed alone is in both.';
COMMENT ON COLUMN billing.ledger_transfers.effective_at IS 'When the transfer takes effect, its business date: when it posted, for the units book; the event''s own instant, for the general ledger. ledger_daily counts it in its 15-minute UTC bucket.';
COMMENT ON COLUMN billing.ledger_transfers.order_line_id IS 'The order line a general-ledger transfer belongs to. No foreign key (ledger purity).';
COMMENT ON COLUMN billing.ledger_transfers.product_id IS 'The product a general-ledger transfer belongs to. No foreign key (ledger purity).';

-- A posting per order line at one coordinate, and usage_reversed's leg in each
-- book: the line and the book are part of its identity.
DROP INDEX billing.ledger_transfers_operation_once_key;
CREATE UNIQUE INDEX ledger_transfers_operation_once_key ON billing.ledger_transfers (merchant_id, customer_id, currency, transfer_type, operation, source, source_id, credit_grant_id, order_line_id, book) NULLS NOT DISTINCT;
COMMENT ON INDEX billing.ledger_transfers_operation_once_key IS 'The structural once-only key for EVERY transfer type. ledger.ApplyIdempotent and books.Post insert ON CONFLICT DO NOTHING against this index, so a replay is refused by the database rather than by a check-then-insert in Go. credit_grant_id, order_line_id and book are part of the identity (one debit per operation per FIFO lot; one posting per order line; usage_reversed''s leg in each book); NULLS NOT DISTINCT because the owed/payment legs carry no lot and system transfers carry no customer.';

-- A posting that creates a merchant-wide general-ledger account nothing
-- opened first raises books.account.opened_late.
ALTER TABLE billing.reconciliation_findings DROP CONSTRAINT reconciliation_findings_type_check,
    ADD CONSTRAINT reconciliation_findings_type_check CHECK (finding_type ~ '^(pull|derive|life|consistency|notify|catalog|books)\.[a-z0-9_]+(\.[a-z0-9_]+)?$');

-- The general ledger records what happened and never refuses a posting for a
-- sign rule: a breach is the finding books.sign_breach, one per account type,
-- currency and (for a customer's account) customer, reopened by a later
-- breach once resolved and kept if ignored.
CREATE FUNCTION billing.record_sign_breach(p_merchant uuid, p_account_type text, p_currency text, p_customer uuid, p_credits bigint, p_debits bigint) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    INSERT INTO billing.reconciliation_findings AS f (merchant_id, finding_type, subject_key, severity, status, recommended_action, evidence)
    VALUES (p_merchant, 'books.sign_breach', p_account_type || '/' || p_currency || coalesce('/' || p_customer::text, ''), 'high', 'requires_review',
            'The general ledger''s ' || p_account_type || ' in ' || p_currency || ' crossed its sign rule. Find the posting that drew it (a reversal of money received before the books, or a writer bound by a balance it cannot read) and correct it.',
            jsonb_strip_nulls(jsonb_build_object('account_type', p_account_type, 'currency', p_currency, 'customer_id', p_customer,
                                                 'credits_posted', p_credits::text, 'debits_posted', p_debits::text)))
    ON CONFLICT (merchant_id, finding_type, psp_id, subject_key) DO UPDATE SET
        status = CASE WHEN f.status = 'ignored' THEN f.status ELSE EXCLUDED.status END,
        resolved_at = CASE WHEN f.status = 'ignored' THEN f.resolved_at END,
        resolution = CASE WHEN f.status = 'ignored' THEN f.resolution END,
        resolved_by = CASE WHEN f.status = 'ignored' THEN f.resolved_by END,
        severity = EXCLUDED.severity, recommended_action = EXCLUDED.recommended_action,
        evidence = EXCLUDED.evidence, last_seen_at = now(), updated_at = now();
END;
$$;

-- A transfer stays in one book and currency, and a customer's account moves
-- only in that customer's transfers.
CREATE FUNCTION billing.ledger_transfers_book_rules() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    debit billing.ledger_accounts%ROWTYPE;
    credit billing.ledger_accounts%ROWTYPE;
BEGIN
    SELECT * INTO debit FROM billing.ledger_accounts WHERE merchant_id = NEW.merchant_id AND id = NEW.debit_account_id;
    SELECT * INTO credit FROM billing.ledger_accounts WHERE merchant_id = NEW.merchant_id AND id = NEW.credit_account_id;
    IF debit.id IS NULL OR credit.id IS NULL THEN
        RAISE EXCEPTION 'ledger_transfers: debit/credit account not found' USING ERRCODE = '23503';
    END IF;
    IF debit.book <> NEW.book OR credit.book <> NEW.book THEN
        RAISE EXCEPTION 'ledger_cross_book: debit %, credit %, transfer %', debit.book, credit.book, NEW.book USING ERRCODE = '23514';
    END IF;
    IF debit.currency <> NEW.currency OR credit.currency <> NEW.currency THEN
        RAISE EXCEPTION 'ledger_cross_currency: debit %, credit %, transfer %', debit.currency, credit.currency, NEW.currency USING ERRCODE = '23514';
    END IF;
    IF debit.customer_id IS NOT NULL AND debit.customer_id IS DISTINCT FROM NEW.customer_id
       OR credit.customer_id IS NOT NULL AND credit.customer_id IS DISTINCT FROM NEW.customer_id THEN
        RAISE EXCEPTION 'ledger_customer_mismatch: transfer customer % moves another customer''s account', NEW.customer_id USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_ledger_transfers_book_rules BEFORE INSERT ON billing.ledger_transfers
    FOR EACH ROW EXECUTE FUNCTION billing.ledger_transfers_book_rules();

-- A merchant-wide general-ledger account keeps no counters, so no transaction
-- holds it from its insert to commit: its balance is its ledger_type_totals
-- row, kept and checked at commit. The accounts that keep counters are locked
-- in id order: the units book refuses a breach of a sign rule, the general
-- ledger records it. The book rules refused a missing account and a
-- cross-currency transfer.
CREATE OR REPLACE FUNCTION billing.ledger_transfers_apply_counters() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'billing', 'pg_catalog' AS $$
DECLARE
    acc billing.ledger_accounts%ROWTYPE;
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    FOR acc IN
        SELECT *
        FROM billing.ledger_accounts
        WHERE merchant_id = NEW.merchant_id
          AND id IN (NEW.debit_account_id, NEW.credit_account_id)
          AND (book = 'units' OR customer_id IS NOT NULL)
        ORDER BY id
        -- Counters do not change account keys. FK checks may already hold
        -- KEY SHARE locks; upgrading those to FOR UPDATE can deadlock peers.
        FOR NO KEY UPDATE
    LOOP
        IF acc.id = NEW.debit_account_id THEN
            IF acc.debits_must_not_exceed_credits AND acc.credits_posted - acc.debits_posted - NEW.amount < -NEW.allow_debit_negative_up_to THEN
                IF acc.book = 'units' THEN
                    RAISE EXCEPTION 'ledger_insufficient_funds: balance %, amount %, floor %', acc.credits_posted - acc.debits_posted, NEW.amount, NEW.allow_debit_negative_up_to;
                END IF;
                PERFORM billing.record_sign_breach(NEW.merchant_id, acc.account_type, acc.currency, acc.customer_id, acc.credits_posted, acc.debits_posted + NEW.amount);
            END IF;
            UPDATE billing.ledger_accounts SET debits_posted = debits_posted + NEW.amount
             WHERE merchant_id = NEW.merchant_id AND id = acc.id;
        ELSE
            IF acc.credits_must_not_exceed_debits AND acc.debits_posted - acc.credits_posted - NEW.amount < 0 THEN
                IF acc.book = 'units' THEN
                    RAISE EXCEPTION 'ledger_credit_constraint: credit account % would exceed debits', acc.id;
                END IF;
                PERFORM billing.record_sign_breach(NEW.merchant_id, acc.account_type, acc.currency, acc.customer_id, acc.credits_posted + NEW.amount, acc.debits_posted);
            END IF;
            UPDATE billing.ledger_accounts SET credits_posted = credits_posted + NEW.amount
             WHERE merchant_id = NEW.merchant_id AND id = acc.id;
        END IF;
    END LOOP;
    RETURN NEW;
END;
$$;

-- Rollups: what every account type holds, and what moved through it in each
-- 15-minute UTC bucket, so a book is read by types and buckets, never by
-- transfers. Every zone's offset is a whole number of quarter hours: a reader
-- groups buckets into the accounting timezone's days, the zone a parameter.
CREATE TABLE billing.ledger_type_totals (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    book text NOT NULL,
    currency text NOT NULL,
    account_type text NOT NULL,
    psp_id uuid,
    credits_posted bigint NOT NULL,
    debits_posted bigint NOT NULL,
    CONSTRAINT ledger_type_totals_account_type_key UNIQUE NULLS NOT DISTINCT (merchant_id, book, currency, account_type, psp_id),
    CONSTRAINT ledger_type_totals_posted_check CHECK (credits_posted >= 0 AND debits_posted >= 0)
);
COMMENT ON TABLE billing.ledger_type_totals IS 'What every account of one type was credited and debited, summed: one row per merchant, book, currency, account type and (cash_clearing) PSP. It is a merchant-wide general-ledger account''s balance, since that account keeps no counters, and settling it refuses the commit when it crosses that account''s sign rule. Kept at commit for the transfers a transaction posted; ledger-audit and the billing restore check recompute it from ledger_transfers.';

CREATE TABLE billing.ledger_daily (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    book text NOT NULL,
    currency text NOT NULL,
    account_type text NOT NULL,
    bucket_at timestamptz NOT NULL,
    transfer_type text NOT NULL,
    psp_id uuid,
    product_id uuid,
    credits bigint NOT NULL,
    debits bigint NOT NULL,
    CONSTRAINT ledger_daily_bucket_key UNIQUE NULLS NOT DISTINCT (merchant_id, book, currency, account_type, bucket_at, transfer_type, psp_id, product_id),
    CONSTRAINT ledger_daily_amounts_check CHECK (credits >= 0 AND debits >= 0)
);
COMMENT ON TABLE billing.ledger_daily IS 'What the transfers of one type credited and debited the accounts of one type in one 15-minute UTC bucket of effective_at: per merchant, book, currency, account type, bucket, transfer type, (cash_clearing) PSP and product. A reader groups buckets into days in the accounting timezone; a value at a past instant is the total less the flows after it. Kept at commit for the transfers a transaction posted; ledger-audit and the billing restore check recompute it from ledger_transfers. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.ledger_daily.bucket_at IS 'The start of the 15-minute UTC bucket: date_bin(''15 minutes'', effective_at, ''2000-01-01 00:00:00+00'').';

-- The transfers already posted fill the rollups, as the trigger below would
-- have.
INSERT INTO billing.ledger_type_totals (merchant_id, book, currency, account_type, psp_id, credits_posted, debits_posted)
SELECT t.merchant_id, t.book, t.currency, a.account_type, a.psp_id,
       sum(CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE 0 END)::bigint,
       sum(CASE WHEN a.id = t.debit_account_id THEN t.amount ELSE 0 END)::bigint
FROM billing.ledger_transfers t
JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
GROUP BY 1, 2, 3, 4, 5;
INSERT INTO billing.ledger_daily (merchant_id, book, currency, account_type, bucket_at, transfer_type, psp_id, product_id, credits, debits)
SELECT t.merchant_id, t.book, t.currency, a.account_type, date_bin('15 minutes', t.effective_at, TIMESTAMPTZ '2000-01-01 00:00:00+00'),
       t.transfer_type, a.psp_id, t.product_id,
       sum(CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE 0 END)::bigint,
       sum(CASE WHEN a.id = t.debit_account_id THEN t.amount ELSE 0 END)::bigint
FROM billing.ledger_transfers t
JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
GROUP BY 1, 2, 3, 4, 5, 6, 7, 8;

-- Only the commit-time settlement writes the rollups.
CREATE FUNCTION billing.guard_ledger_rollups() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP = 'DELETE' OR pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION '% is maintained by transfer insertion', TG_TABLE_NAME USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER guard_ledger_type_totals BEFORE INSERT OR DELETE OR UPDATE ON billing.ledger_type_totals
    FOR EACH ROW EXECUTE FUNCTION billing.guard_ledger_rollups();
CREATE TRIGGER immutable_ledger_type_totals_truncate BEFORE TRUNCATE ON billing.ledger_type_totals
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER guard_ledger_daily BEFORE INSERT OR DELETE OR UPDATE ON billing.ledger_daily
    FOR EACH ROW EXECUTE FUNCTION billing.guard_ledger_rollups();
CREATE TRIGGER immutable_ledger_daily_truncate BEFORE TRUNCATE ON billing.ledger_daily
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- The rollups are kept at commit, once every account the transaction moves is
-- locked: a transfer only queues itself here, and the first deferred firing
-- folds every queued transfer in, locking rollup rows in one order.
CREATE TABLE billing.ledger_rollup_pending (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    transfer_id uuid NOT NULL,
    PRIMARY KEY (merchant_id, transfer_id)
);
COMMENT ON TABLE billing.ledger_rollup_pending IS 'Transfers this transaction posted whose rollups it has not yet kept: transaction scratch, empty at commit.';

CREATE FUNCTION billing.ledger_transfers_queue_rollups() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    INSERT INTO billing.ledger_rollup_pending (merchant_id, transfer_id) VALUES (NEW.merchant_id, NEW.id);
    RETURN NULL;
END;
$$;

CREATE TRIGGER trg_ledger_transfers_rollups AFTER INSERT ON billing.ledger_transfers
    FOR EACH ROW EXECUTE FUNCTION billing.ledger_transfers_queue_rollups();

-- Folds a merchant's queued transfers into the rollups: each total, then each
-- bucket, in key order, then empties the queue. A merchant-wide general-ledger
-- account's total this settlement moved, locked here, that crosses the
-- account's sign rule raises books.sign_breach; it never refuses the commit.
CREATE FUNCTION billing.settle_ledger_rollups(p_merchant uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    INSERT INTO billing.ledger_type_totals AS r (merchant_id, book, currency, account_type, psp_id, credits_posted, debits_posted)
    SELECT p_merchant, t.book, t.currency, a.account_type, a.psp_id,
           sum(CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE 0 END)::bigint,
           sum(CASE WHEN a.id = t.debit_account_id THEN t.amount ELSE 0 END)::bigint
      FROM billing.ledger_rollup_pending p
      JOIN billing.ledger_transfers t ON t.merchant_id = p.merchant_id AND t.id = p.transfer_id
      JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
     WHERE p.merchant_id = p_merchant
     GROUP BY 2, 3, 4, 5
     ORDER BY 2, 3, 4, 5
    ON CONFLICT (merchant_id, book, currency, account_type, psp_id) DO UPDATE
    SET credits_posted = r.credits_posted + EXCLUDED.credits_posted, debits_posted = r.debits_posted + EXCLUDED.debits_posted;
    PERFORM billing.record_sign_breach(p_merchant, t.account_type, t.currency, NULL, t.credits_posted, t.debits_posted)
       FROM billing.ledger_type_totals t
       JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.book = t.book AND a.customer_id IS NULL
        AND a.account_type = t.account_type AND a.currency = t.currency AND a.psp_id IS NOT DISTINCT FROM t.psp_id
      WHERE t.merchant_id = p_merchant AND t.book = 'general'
        AND (a.debits_must_not_exceed_credits AND t.debits_posted > t.credits_posted
             OR a.credits_must_not_exceed_debits AND t.credits_posted > t.debits_posted)
        AND EXISTS (SELECT 1 FROM billing.ledger_rollup_pending p
                      JOIN billing.ledger_transfers x ON x.merchant_id = p.merchant_id AND x.id = p.transfer_id
                     WHERE p.merchant_id = p_merchant AND a.id IN (x.debit_account_id, x.credit_account_id))
      ORDER BY t.account_type, t.currency;
    INSERT INTO billing.ledger_daily AS r (merchant_id, book, currency, account_type, bucket_at, transfer_type, psp_id, product_id, credits, debits)
    SELECT p_merchant, t.book, t.currency, a.account_type, date_bin('15 minutes', t.effective_at, TIMESTAMPTZ '2000-01-01 00:00:00+00'),
           t.transfer_type, a.psp_id, t.product_id,
           sum(CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE 0 END)::bigint,
           sum(CASE WHEN a.id = t.debit_account_id THEN t.amount ELSE 0 END)::bigint
      FROM billing.ledger_rollup_pending p
      JOIN billing.ledger_transfers t ON t.merchant_id = p.merchant_id AND t.id = p.transfer_id
      JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
     WHERE p.merchant_id = p_merchant
     GROUP BY 2, 3, 4, 5, 6, 7, 8
     ORDER BY 2, 3, 4, 5, 6, 7, 8
    ON CONFLICT (merchant_id, book, currency, account_type, bucket_at, transfer_type, psp_id, product_id) DO UPDATE
    SET credits = r.credits + EXCLUDED.credits, debits = r.debits + EXCLUDED.debits;
    DELETE FROM billing.ledger_rollup_pending WHERE merchant_id = p_merchant;
END;
$$;

-- The event log's flush, as billing.eventlog_flush did it: numbers the
-- transaction's staged events into their streams, each stream locked in
-- (source, scope) order and notified at commit.
CREATE FUNCTION billing.flush_event_log(p_schema text) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'pg_temp' AS $$
DECLARE
 s record; e record; next_position bigint; stamp timestamptz;
BEGIN
 -- Every scope touched by this transaction is known before taking any locks.
 FOR s IN SELECT DISTINCT source,scope FROM "billing".eventlog_pending
          WHERE transaction_id=pg_current_xact_id() ORDER BY source,scope
 LOOP
  INSERT INTO "billing".eventlog_streams(source,scope) VALUES(s.source,s.scope) ON CONFLICT DO NOTHING;
  PERFORM 1 FROM "billing".eventlog_streams WHERE source=s.source AND scope=s.scope FOR UPDATE;
  -- Delivered at commit, so a LISTENing consumer reads when its stream has moved (version 2).
  PERFORM pg_notify(p_schema || '.eventlog', s.source || ' ' || s.scope);
 END LOOP;
 FOR e IN SELECT * FROM "billing".eventlog_pending WHERE transaction_id=pg_current_xact_id() ORDER BY ordinal
 LOOP
  IF EXISTS(SELECT 1 FROM "billing".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id) THEN
   IF EXISTS(SELECT 1 FROM "billing".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id
     AND event_source=e.event_source AND event_type=e.event_type AND version=e.version
     AND occurred_at=e.occurred_at AND subject_type=e.subject_type AND subject_id=e.subject_id AND data=e.data) THEN
    CONTINUE;
   END IF;
   RAISE EXCEPTION 'event ID already records a different fact' USING ERRCODE='23505';
  END IF;
  UPDATE "billing".eventlog_streams SET position=position+1,head_id=coalesce(e.boundary,e.id),
   recorded_at=greatest(recorded_at,clock_timestamp())
   WHERE source=e.source AND scope=e.scope RETURNING position,recorded_at INTO next_position,stamp;
  INSERT INTO "billing".eventlog_events(source,scope,position,id,boundary,event_source,event_type,version,occurred_at,recorded_at,subject_type,subject_id,data)
   VALUES(e.source,e.scope,next_position,e.id,coalesce(e.boundary,e.id),e.event_source,e.event_type,e.version,e.occurred_at,stamp,e.subject_type,e.subject_id,e.data);
 END LOOP;
 DELETE FROM "billing".eventlog_pending WHERE transaction_id=pg_current_xact_id();
END $$;

-- Everything a transaction keeps at commit, after every statement-time lock,
-- in one order: each merchant's ledger rollups, then each merchant's MRR,
-- merchants in id order, again until neither queue holds work, then the event
-- streams. A new commit-time step takes its place in this sequence. Ledger or
-- MRR work queued once the streams are flushing would lock out of order: it
-- is refused.
CREATE FUNCTION billing.settle_commit(p_schema text) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    v_merchant uuid;
BEGIN
    IF current_setting('openrails.event_log_flushed', true) = 'true'
       AND (EXISTS (SELECT 1 FROM billing.ledger_rollup_pending) OR EXISTS (SELECT 1 FROM billing.subscription_mrr WHERE dirty)) THEN
        RAISE EXCEPTION 'settle_commit: ledger or MRR work queued after the event log flushed' USING ERRCODE = '55000';
    END IF;
    LOOP
        FOR v_merchant IN SELECT DISTINCT merchant_id FROM billing.ledger_rollup_pending ORDER BY merchant_id LOOP
            PERFORM billing.settle_ledger_rollups(v_merchant);
        END LOOP;
        FOR v_merchant IN SELECT DISTINCT merchant_id FROM billing.subscription_mrr WHERE dirty ORDER BY merchant_id LOOP
            PERFORM billing.settle_mrr(v_merchant);
        END LOOP;
        EXIT WHEN NOT EXISTS (SELECT 1 FROM billing.ledger_rollup_pending)
              AND NOT EXISTS (SELECT 1 FROM billing.subscription_mrr WHERE dirty);
    END LOOP;
    PERFORM set_config('openrails.event_log_flushed', 'true', true);
    PERFORM billing.flush_event_log(p_schema);
    IF EXISTS (SELECT 1 FROM billing.ledger_rollup_pending) OR EXISTS (SELECT 1 FROM billing.subscription_mrr WHERE dirty) THEN
        RAISE EXCEPTION 'settle_commit: ledger or MRR work queued after the event log flushed' USING ERRCODE = '55000';
    END IF;
END;
$$;

-- Every commit-time queue fires this one settler: the first event whose row
-- is still unsettled runs the whole sequence; every later one finds its row
-- settled and returns.
CREATE FUNCTION billing.settle_at_commit() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_TABLE_NAME = 'ledger_rollup_pending' THEN
        PERFORM 1 FROM billing.ledger_rollup_pending WHERE merchant_id = NEW.merchant_id AND transfer_id = NEW.transfer_id;
    ELSIF TG_TABLE_NAME = 'subscription_mrr' THEN
        PERFORM 1 FROM billing.subscription_mrr WHERE merchant_id = NEW.merchant_id AND subscription_id = NEW.subscription_id AND dirty;
    ELSE
        PERFORM 1 FROM billing.eventlog_pending WHERE ordinal = NEW.ordinal;
    END IF;
    IF FOUND THEN
        PERFORM billing.settle_commit(TG_TABLE_SCHEMA);
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_rollups_settle AFTER INSERT ON billing.ledger_rollup_pending DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing.settle_at_commit();
DROP TRIGGER subscription_mrr_settle ON billing.subscription_mrr;
DROP FUNCTION billing.settle_subscription_mrr();
CREATE CONSTRAINT TRIGGER subscription_mrr_settle AFTER INSERT OR UPDATE ON billing.subscription_mrr DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing.settle_at_commit();
DROP TRIGGER eventlog_flush_at_commit ON billing.eventlog_pending;
DROP FUNCTION billing.eventlog_flush();
CREATE CONSTRAINT TRIGGER eventlog_flush_at_commit AFTER INSERT ON billing.eventlog_pending DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing.settle_at_commit();

-- A restore settles its rollups before the receipt's commit check reads them.
CREATE OR REPLACE FUNCTION billing.finish_billing_restore(p_merchant uuid, p_digest text, p_rows bigint) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
BEGIN
    IF NOT billing.billing_restore_active(p_merchant) OR p_digest IS NULL OR p_rows IS NULL OR p_digest !~ '^[0-9a-f]{64}$' OR p_rows<0 THEN
        RAISE EXCEPTION 'invalid billing restore finalization' USING ERRCODE='23514';
    END IF;
    SET CONSTRAINTS billing.ledger_rollups_settle IMMEDIATE;
    UPDATE billing.maintenance_runs SET status='completed',finished_at=now(),summary=jsonb_build_object('digest',p_digest,'rows',p_rows)
        WHERE merchant_id=p_merchant AND kind='billing_restore' AND id::text=current_setting('openrails.billing_restore_id',true);
END;
$_$;

-- A restored ledger proves its counters (0 on a merchant-wide general-ledger
-- account), its conservation per book and currency (such an account's balance
-- is its type total), each transfer's agreement with its accounts and its
-- rollups.
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
           WHERE a.merchant_id=p_merchant GROUP BY a.book, a.currency
           HAVING sum(CASE WHEN a.book='general' AND a.customer_id IS NULL THEN coalesce(t.credits_posted::numeric-t.debits_posted::numeric,0)
                           ELSE a.credits_posted::numeric-a.debits_posted::numeric END)<>0)
       OR EXISTS (SELECT 1 FROM billing.ledger_transfers t
           JOIN billing.ledger_accounts d ON d.merchant_id=t.merchant_id AND d.id=t.debit_account_id
           JOIN billing.ledger_accounts c ON c.merchant_id=t.merchant_id AND c.id=t.credit_account_id WHERE t.merchant_id=p_merchant
           AND (d.currency<>t.currency OR c.currency<>t.currency OR d.book<>t.book OR c.book<>t.book
                OR (d.customer_id IS NOT NULL AND d.customer_id IS DISTINCT FROM t.customer_id)
                OR (c.customer_id IS NOT NULL AND c.customer_id IS DISTINCT FROM t.customer_id)))
       OR EXISTS (WITH legs AS (SELECT t.book, t.currency, a.account_type, a.psp_id,
                CASE WHEN a.id=t.credit_account_id THEN t.amount ELSE 0 END AS credit,
                CASE WHEN a.id=t.debit_account_id THEN t.amount ELSE 0 END AS debit
                FROM billing.ledger_transfers t JOIN billing.ledger_accounts a ON a.merchant_id=t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
                WHERE t.merchant_id=p_merchant),
            logged AS (SELECT book, currency, account_type, psp_id, sum(credit)::bigint AS credits, sum(debit)::bigint AS debits FROM legs GROUP BY 1, 2, 3, 4),
            kept AS (SELECT book, currency, account_type, psp_id, credits_posted, debits_posted FROM billing.ledger_type_totals WHERE merchant_id=p_merchant)
           (SELECT * FROM logged EXCEPT SELECT * FROM kept) UNION ALL (SELECT * FROM kept EXCEPT SELECT * FROM logged))
       OR EXISTS (WITH legs AS (SELECT t.book, t.currency, a.account_type,
                date_bin('15 minutes', t.effective_at, TIMESTAMPTZ '2000-01-01 00:00:00+00') AS bucket_at, t.transfer_type, a.psp_id, t.product_id,
                CASE WHEN a.id=t.credit_account_id THEN t.amount ELSE 0 END AS credit,
                CASE WHEN a.id=t.debit_account_id THEN t.amount ELSE 0 END AS debit
                FROM billing.ledger_transfers t JOIN billing.ledger_accounts a ON a.merchant_id=t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
                WHERE t.merchant_id=p_merchant),
            logged AS (SELECT book, currency, account_type, bucket_at, transfer_type, psp_id, product_id, sum(credit)::bigint AS credits, sum(debit)::bigint AS debits FROM legs GROUP BY 1, 2, 3, 4, 5, 6, 7),
            kept AS (SELECT book, currency, account_type, bucket_at, transfer_type, psp_id, product_id, credits, debits FROM billing.ledger_daily WHERE merchant_id=p_merchant)
           (SELECT * FROM logged EXCEPT SELECT * FROM kept) UNION ALL (SELECT * FROM kept EXCEPT SELECT * FROM logged)) THEN
        RAISE EXCEPTION 'billing restore ledger integrity mismatch' USING ERRCODE='23514';
    END IF;
END;
$$;

-- A billing restore's destination holds none of the merchant's rollups.
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
