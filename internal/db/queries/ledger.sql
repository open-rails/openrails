-- The double-entry money ledger (ledger_accounts + ledger_transfers). Balances
-- are maintained counters on accounts; transfers are append-only.

-- name: GetLedgerAccount :one
SELECT * FROM billing.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND account_type = sqlc.arg(account_type)::text
  AND currency = sqlc.arg(currency)::text
  AND customer_id IS NOT DISTINCT FROM sqlc.narg(customer_id)::uuid;

-- name: InsertLedgerAccount :one
INSERT INTO billing.ledger_accounts (
    merchant_id, customer_id, account_type, currency,
    debits_must_not_exceed_credits, credits_must_not_exceed_debits
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.narg(customer_id)::uuid,
    sqlc.arg(account_type)::text, sqlc.arg(currency)::text,
    sqlc.arg(debits_must_not_exceed_credits)::boolean, sqlc.arg(credits_must_not_exceed_debits)::boolean
)
RETURNING *;

-- InsertLedgerTransfer is the one durable money write. ON CONFLICT DO NOTHING
-- on ledger_transfers_operation_once_key makes once-only a database fact: a
-- replay at the same coordinate inserts nothing and returns zero rows, whatever
-- the caller's lock order. Zero rows means already applied: ApplyIdempotent
-- reads the committed row.
-- name: InsertLedgerTransfer :one
INSERT INTO billing.ledger_transfers (
    merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type,
    allow_debit_negative_up_to, operation,
    source, source_id, grant_id, customer_id, invoker_id, resource, invoice_id
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(debit_account_id)::uuid, sqlc.arg(credit_account_id)::uuid,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(transfer_type)::text,
    sqlc.arg(allow_debit_negative_up_to)::bigint, sqlc.arg(operation)::text,
    sqlc.arg(source)::text, sqlc.arg(source_id)::text, sqlc.narg(grant_id)::uuid, sqlc.narg(customer_id)::uuid,
    sqlc.narg(invoker_id)::text, sqlc.narg(resource)::text, sqlc.narg(invoice_id)::uuid
)
ON CONFLICT (merchant_id, customer_id, currency, transfer_type, operation, source, source_id, grant_id)
DO NOTHING
RETURNING *;

-- GetLedgerTransferAtCoordinate reads the row a conflicting insert lost to —
-- the full physical identity, lot included, so a multi-lot spend resolves the
-- right leg.
-- name: GetLedgerTransferAtCoordinate :one
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id IS NOT DISTINCT FROM sqlc.narg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type = sqlc.arg(transfer_type)::text
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text
  AND grant_id IS NOT DISTINCT FROM sqlc.narg(grant_id)::uuid;

-- LedgerAccountBalance: net credit (credits - debits) from the maintained
-- account counters.
-- name: LedgerAccountBalance :one
SELECT (credits_posted - debits_posted)::bigint AS balance
FROM billing.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(account_id)::uuid;

-- name: ListLedgerTransfersByCustomer :many
-- A customer's movements in one currency, newest first.
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND (sqlc.narg(after_at)::timestamptz IS NULL
   OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListLedgerTransfersByIDs :many
-- A customer's named movements, newest first.
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
  AND customer_id = sqlc.arg(customer_id)::uuid
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
  AND transfer_type IN ('credit_spend', 'spend', 'owed_accrual')
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text
ORDER BY created_at ASC
LIMIT 1;

-- SumLedgerMovementsByCustomerInPeriod: per-type net amount for a customer in a
-- window — the invoice builder's money-movement rollup. Spends/expiries are
-- emitted as positive transfer amounts; the sign is applied by the caller.
-- name: SumLedgerMovementsByCustomerInPeriod :many
SELECT transfer_type, COALESCE(SUM(amount), 0)::bigint AS total
FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND created_at >= sqlc.arg(period_starts_at)::timestamptz
  AND created_at < sqlc.arg(period_ends_at)::timestamptz
GROUP BY transfer_type;

-- ListLedgerConservationBreaches is an on-demand integrity diagnostic. It must
-- return every breached ledger so the operator cannot receive a false healthy
-- result from pagination.
-- name: ListLedgerConservationBreaches :many
SELECT merchant_id,
       currency,
       SUM(credits_posted - debits_posted)::bigint AS net,
       COUNT(*)::bigint AS accounts
FROM billing.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
GROUP BY merchant_id, currency
HAVING SUM(credits_posted - debits_posted) <> 0
ORDER BY merchant_id, currency;

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
  AND transfer_type IN ('credit_spend', 'spend', 'owed_accrual')
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text;
