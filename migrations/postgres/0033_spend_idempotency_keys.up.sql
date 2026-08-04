-- or#891 item 2: give the BALANCE leg of a spend a structural idempotency key.
--
-- 0001 shipped idx_ledger_transfers_owed_accrual_once, so the owed leg of a
-- spend could only be accrued once. The prepaid-balance debit — the common path
-- — was covered by nothing: its once-only property rested entirely on
-- lockBalance running before the SELECT-then-INSERT in Go. A spend path that
-- omits lockBalance compiles, passes single-threaded tests, and double-charges
-- only under concurrency.
--
-- CreditSpend emits one credit_spend transfer PER FIFO credit lot drawn, so the
-- operation coordinate alone is not unique — the lot is part of the physical
-- row identity. (merchant, customer, currency, source, source_id, grant_id) is
-- therefore the tightest true key: one debit per (operation, lot).
--
-- The `source_id <> ''` predicate is also dropped from the owed index. It
-- encoded "an empty key means don't dedupe" in the SCHEMA, a second time and
-- independently of the Go-side guards or#891 item 1 removes; leaving it would
-- keep the escape hatch open after the code closed it. Every producer of these
-- transfer types now hard-requires a non-empty key.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE UNIQUE INDEX idx_ledger_transfers_credit_spend_once
    ON openrails.ledger_transfers (merchant_id, customer_id, currency, source, source_id, grant_id)
    WHERE transfer_type = 'credit_spend';

DROP INDEX openrails.idx_ledger_transfers_owed_accrual_once;

CREATE UNIQUE INDEX idx_ledger_transfers_owed_accrual_once
    ON openrails.ledger_transfers (merchant_id, customer_id, currency, source, source_id)
    WHERE transfer_type = 'owed_accrual';
