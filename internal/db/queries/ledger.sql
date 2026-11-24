-- The books (ledger_accounts + ledger_transfers): the units book and the
-- general ledger. Balances are maintained counters on accounts; transfers are
-- append-only.

-- name: GetLedgerAccount :one
SELECT * FROM billing.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND account_type = sqlc.arg(account_type)::text
  AND currency = sqlc.arg(currency)::text
  AND customer_id IS NOT DISTINCT FROM sqlc.narg(customer_id)::uuid
  AND psp_id IS NOT DISTINCT FROM sqlc.narg(psp_id)::uuid;

-- name: InsertLedgerAccount :one
INSERT INTO billing.ledger_accounts (
    merchant_id, book, class, customer_id, psp_id, account_type, currency,
    debits_must_not_exceed_credits, credits_must_not_exceed_debits
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(book)::text, sqlc.arg(class)::text,
    sqlc.narg(customer_id)::uuid, sqlc.narg(psp_id)::uuid,
    sqlc.arg(account_type)::text, sqlc.arg(currency)::text,
    sqlc.arg(debits_must_not_exceed_credits)::boolean, sqlc.arg(credits_must_not_exceed_debits)::boolean
)
ON CONFLICT DO NOTHING
RETURNING *;

-- InsertLedgerTransfer is the units book's one durable write, effective when
-- it posts. ON CONFLICT DO NOTHING on
-- ledger_transfers_operation_once_key makes once-only a database fact: a
-- replay at the same coordinate inserts nothing and returns zero rows, whatever
-- the caller's lock order. Zero rows means already applied: ApplyIdempotent
-- reads the committed row.
-- name: InsertLedgerTransfer :one
INSERT INTO billing.ledger_transfers (
    merchant_id, book, debit_account_id, credit_account_id, amount, currency, transfer_type,
    allow_debit_negative_up_to, operation,
    source, source_id, credit_grant_id, customer_id, invoker_id, resource, order_id,
    effective_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, 'units', sqlc.arg(debit_account_id)::uuid, sqlc.arg(credit_account_id)::uuid,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(transfer_type)::text,
    0, sqlc.arg(operation)::text,
    sqlc.arg(source)::text, sqlc.arg(source_id)::text, sqlc.narg(credit_grant_id)::uuid, sqlc.narg(customer_id)::uuid,
    sqlc.narg(invoker_id)::text, sqlc.narg(resource)::text, sqlc.narg(order_id)::uuid,
    now()
)
ON CONFLICT (merchant_id, customer_id, currency, transfer_type, operation, source, source_id, credit_grant_id, order_line_id)
DO NOTHING
RETURNING *;

-- InsertGeneralLedgerTransfer is the general ledger's one durable write, at
-- the instant its poster names. No floor relaxes a sign rule. Once-only as
-- InsertLedgerTransfer.
-- name: InsertGeneralLedgerTransfer :one
INSERT INTO billing.ledger_transfers (
    merchant_id, book, debit_account_id, credit_account_id, amount, currency, transfer_type,
    allow_debit_negative_up_to, operation, source, source_id, credit_grant_id, customer_id,
    invoker_id, resource, order_id, order_line_id, product_id, effective_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, 'general', sqlc.arg(debit_account_id)::uuid, sqlc.arg(credit_account_id)::uuid,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(transfer_type)::text,
    0, sqlc.arg(operation)::text, sqlc.arg(source)::text, sqlc.arg(source_id)::text,
    sqlc.narg(credit_grant_id)::uuid, sqlc.narg(customer_id)::uuid,
    sqlc.narg(invoker_id)::text, sqlc.narg(resource)::text, sqlc.narg(order_id)::uuid,
    sqlc.narg(order_line_id)::uuid, sqlc.narg(product_id)::uuid,
    sqlc.arg(effective_at)::timestamptz
)
ON CONFLICT (merchant_id, customer_id, currency, transfer_type, operation, source, source_id, credit_grant_id, order_line_id)
DO NOTHING
RETURNING *;

-- GetLedgerTransferAtCoordinate reads the row a conflicting insert lost to —
-- the full physical identity, lot and order line included, so a multi-lot
-- spend resolves the right leg.
-- name: GetLedgerTransferAtCoordinate :one
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id IS NOT DISTINCT FROM sqlc.narg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type = sqlc.arg(transfer_type)::text
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text
  AND credit_grant_id IS NOT DISTINCT FROM sqlc.narg(credit_grant_id)::uuid
  AND order_line_id IS NOT DISTINCT FROM sqlc.narg(order_line_id)::uuid;

-- LedgerAccountBalance: net credit (credits - debits) from the maintained
-- account counters.
-- name: LedgerAccountBalance :one
SELECT (credits_posted - debits_posted)::bigint AS balance
FROM billing.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(account_id)::uuid;

-- name: ListLedgerTransfersByCustomer :many
-- A customer's units-book movements in one currency, newest first.
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND book = 'units'
  AND (sqlc.narg(after_at)::timestamptz IS NULL
   OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListLedgerTransfersByIDs :many
-- A customer's named units-book movements, newest first.
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
  AND customer_id = sqlc.arg(customer_id)::uuid AND book = 'units'
ORDER BY created_at DESC, id DESC;

-- GetLedgerTransferByCoords: the latest transfer at the full operation
-- coordinate. `operation` keeps a capture and a wasted-spend usage charge that
-- share one (source, source_id) apart.
-- name: GetLedgerTransferByCoords :one
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type = sqlc.arg(transfer_type)::text
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text
ORDER BY created_at DESC
LIMIT 1;

-- GetLedgerSpendByCoords: the first posted spend movement for one money
-- operation at its idempotency coordinate.
-- name: GetLedgerSpendByCoords :one
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type IN ('credit_spend', 'owed_accrual')
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text
ORDER BY created_at ASC
LIMIT 1;

-- ListLedgerConservationBreaches: every (book, currency) whose balances do
-- not sum to zero.
-- name: ListLedgerConservationBreaches :many
SELECT merchant_id,
       book,
       currency,
       SUM(credits_posted - debits_posted)::bigint AS net,
       COUNT(*)::bigint AS accounts
FROM billing.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
GROUP BY merchant_id, book, currency
HAVING SUM(credits_posted - debits_posted) <> 0
ORDER BY merchant_id, book, currency;

-- ListLedgerCounterDrifts rebuilds account counters from the immutable
-- transfer log and returns every account whose maintained projection differs.
-- name: ListLedgerCounterDrifts :many
WITH logged AS (
    SELECT account_id, SUM(credit)::bigint AS credits, SUM(debit)::bigint AS debits
    FROM (
        SELECT credit_account_id AS account_id, amount AS credit, 0::bigint AS debit
        FROM billing.ledger_transfers
        WHERE merchant_id = sqlc.arg(merchant_id)::uuid
        UNION ALL
        SELECT debit_account_id, 0::bigint, amount
        FROM billing.ledger_transfers
        WHERE merchant_id = sqlc.arg(merchant_id)::uuid
    ) legs
    GROUP BY account_id
)
SELECT a.id AS account_id,
       a.merchant_id,
       a.book,
       a.currency,
       a.account_type,
       a.customer_id,
       a.credits_posted AS stored_credits,
       COALESCE(l.credits, 0)::bigint AS logged_credits,
       a.debits_posted AS stored_debits,
       COALESCE(l.debits, 0)::bigint AS logged_debits
FROM billing.ledger_accounts a
LEFT JOIN logged l ON l.account_id = a.id
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (a.credits_posted <> COALESCE(l.credits, 0)
    OR a.debits_posted <> COALESCE(l.debits, 0))
ORDER BY a.merchant_id, a.currency, a.id;

-- SumLedgerSpendByCoords: the total posted at one operation coordinate. A spend
-- fans out into one credit_spend per FIFO lot drawn plus at most one
-- owed_accrual, so only the sum is the operation's amount: a retry carrying a
-- different amount reuses the key with a changed body.
-- name: SumLedgerSpendByCoords :one
SELECT COALESCE(SUM(amount), 0)::bigint AS total, count(*)::bigint AS transfers
FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type IN ('credit_spend', 'owed_accrual')
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text;

-- ListLedgerTypeTotalDrifts recomputes ledger_type_totals from the transfer
-- log and returns every key whose kept total differs; a missing row keeps 0.
-- name: ListLedgerTypeTotalDrifts :many
WITH legs AS (
    SELECT t.book, t.currency, a.account_type, a.psp_id,
           CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE 0 END AS credit,
           CASE WHEN a.id = t.debit_account_id THEN t.amount ELSE 0 END AS debit
    FROM billing.ledger_transfers t
    JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
    WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid
), sides AS (
    SELECT book, currency, account_type, psp_id, 0::bigint AS stored_credits, 0::bigint AS stored_debits, credit AS logged_credits, debit AS logged_debits
    FROM legs
    UNION ALL
    SELECT book, currency, account_type, psp_id, credits_posted, debits_posted, 0, 0
    FROM billing.ledger_type_totals
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
)
SELECT book::text AS book,
       currency::text AS currency,
       account_type::text AS account_type,
       psp_id,
       SUM(stored_credits)::bigint AS stored_credits,
       SUM(logged_credits)::bigint AS logged_credits,
       SUM(stored_debits)::bigint AS stored_debits,
       SUM(logged_debits)::bigint AS logged_debits
FROM sides
GROUP BY book, currency, account_type, psp_id
HAVING SUM(stored_credits) <> SUM(logged_credits) OR SUM(stored_debits) <> SUM(logged_debits)
ORDER BY book, currency, account_type, psp_id;

-- ListLedgerDailyDrifts recomputes ledger_daily from the transfer log and
-- returns every bucket whose kept amounts differ; a missing row keeps 0.
-- name: ListLedgerDailyDrifts :many
WITH legs AS (
    SELECT t.book, t.currency, a.account_type,
           date_bin('15 minutes', t.effective_at, '2000-01-01 00:00:00+00'::timestamptz) AS bucket_at,
           t.transfer_type, a.psp_id, t.product_id,
           CASE WHEN a.id = t.credit_account_id THEN t.amount ELSE 0 END AS credit,
           CASE WHEN a.id = t.debit_account_id THEN t.amount ELSE 0 END AS debit
    FROM billing.ledger_transfers t
    JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
    WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid
), sides AS (
    SELECT book, currency, account_type, bucket_at, transfer_type, psp_id, product_id,
           0::bigint AS stored_credits, 0::bigint AS stored_debits, credit AS logged_credits, debit AS logged_debits
    FROM legs
    UNION ALL
    SELECT book, currency, account_type, bucket_at, transfer_type, psp_id, product_id, credits, debits, 0, 0
    FROM billing.ledger_daily
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
)
SELECT book::text AS book,
       currency::text AS currency,
       account_type::text AS account_type,
       bucket_at::timestamptz AS bucket_at,
       transfer_type::text AS transfer_type,
       psp_id,
       product_id,
       SUM(stored_credits)::bigint AS stored_credits,
       SUM(logged_credits)::bigint AS logged_credits,
       SUM(stored_debits)::bigint AS stored_debits,
       SUM(logged_debits)::bigint AS logged_debits
FROM sides
GROUP BY book, currency, account_type, bucket_at, transfer_type, psp_id, product_id
HAVING SUM(stored_credits) <> SUM(logged_credits) OR SUM(stored_debits) <> SUM(logged_debits)
ORDER BY book, currency, account_type, bucket_at, transfer_type, psp_id, product_id;
