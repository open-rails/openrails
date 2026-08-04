-- #512 double-entry immutable money ledger (ledger_accounts + ledger_transfers).
-- Balances are O(1) maintained counters on accounts; transfers are append-only.

-- name: GetLedgerAccount :one
SELECT * FROM openrails.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND account_type = sqlc.arg(account_type)::text
  AND currency = sqlc.arg(currency)::text
  AND customer_id IS NOT DISTINCT FROM sqlc.narg(customer_id)::uuid;

-- name: InsertLedgerAccount :one
INSERT INTO openrails.ledger_accounts (
    merchant_id, customer_id, account_type, currency,
    debits_must_not_exceed_credits, credits_must_not_exceed_debits
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.narg(customer_id)::uuid,
    sqlc.arg(account_type)::text, sqlc.arg(currency)::text,
    sqlc.arg(debits_must_not_exceed_credits)::boolean, sqlc.arg(credits_must_not_exceed_debits)::boolean
)
RETURNING *;

-- name: InsertLedgerTransfer :one
INSERT INTO openrails.ledger_transfers (
    merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type,
    allow_debit_negative_up_to, operation,
    source, source_id, grant_id, customer_id, invoker_id, resource, invoice_id
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(debit_account_id)::uuid, sqlc.arg(credit_account_id)::uuid,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(transfer_type)::text,
    sqlc.arg(allow_debit_negative_up_to)::bigint, sqlc.arg(operation)::text,
    sqlc.narg(source)::text, sqlc.narg(source_id)::text, sqlc.narg(grant_id)::uuid, sqlc.narg(customer_id)::uuid,
    sqlc.narg(invoker_id)::text, sqlc.narg(resource)::text, sqlc.narg(invoice_id)::uuid
)
RETURNING *;

-- name: GetLedgerAccountByID :one
SELECT * FROM openrails.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- LedgerAccountBalance: net credit (credits - debits) from maintained account
-- counters. This is the Phase H O(1) replacement for summing ledger_transfers.
-- name: LedgerAccountBalance :one
SELECT (credits_posted - debits_posted)::bigint AS balance
FROM openrails.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(account_id)::uuid;

-- ListLedgerTransfersByCustomer: a customer's money-movement history (newest
-- first, paginated) — the source for GetTransactions after the single-entry
-- money_transactions table was retired (#512 hard cut).
-- name: ListLedgerTransfersByCustomer :many
SELECT * FROM openrails.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
ORDER BY created_at DESC
LIMIT sqlc.arg(lim)::int OFFSET sqlc.arg(off)::int;

-- name: CountLedgerTransfersByCustomer :one
SELECT count(*) FROM openrails.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text;

-- GetLedgerTransferByCoords: idempotency / lookup by the FULL operation
-- coordinate (merchant, customer, currency, transfer_type, operation, source,
-- source_id). `operation` is the or#894 discriminator: without it a capture and
-- a wasted-spend usage charge sharing one (source, source_id) alias here.
-- Newest-first so a replay returns the latest row.
-- name: GetLedgerTransferByCoords :one
SELECT * FROM openrails.ledger_transfers
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
SELECT * FROM openrails.ledger_transfers
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
FROM openrails.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND created_at >= sqlc.arg(period_from)::timestamptz
  AND created_at < sqlc.arg(period_to)::timestamptz
GROUP BY transfer_type;

-- ListLedgerConservationBreaches is an on-demand integrity diagnostic. It must
-- return every breached ledger so the operator cannot receive a false healthy
-- result from pagination.
-- name: ListLedgerConservationBreaches :many
SELECT merchant_id,
       currency,
       SUM(credits_posted - debits_posted)::bigint AS net,
       COUNT(*)::bigint AS accounts
FROM openrails.ledger_accounts
WHERE (sqlc.narg(merchant_id)::uuid IS NULL
    OR merchant_id = sqlc.narg(merchant_id)::uuid)
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
        FROM openrails.ledger_transfers
        WHERE (sqlc.narg(merchant_id)::uuid IS NULL
            OR merchant_id = sqlc.narg(merchant_id)::uuid)
        UNION ALL
        SELECT debit_account_id, 0::bigint, amount
        FROM openrails.ledger_transfers
        WHERE (sqlc.narg(merchant_id)::uuid IS NULL
            OR merchant_id = sqlc.narg(merchant_id)::uuid)
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
FROM openrails.ledger_accounts a
LEFT JOIN logged l ON l.account_id = a.id
WHERE (sqlc.narg(merchant_id)::uuid IS NULL
    OR a.merchant_id = sqlc.narg(merchant_id)::uuid)
  AND (a.credits_posted <> COALESCE(l.credits, 0)
    OR a.debits_posted <> COALESCE(l.debits, 0))
ORDER BY a.merchant_id, a.currency, a.id;

-- SumLedgerSpendByCoords: the TOTAL money already posted at one operation
-- coordinate. A spend fans out into one credit_spend transfer per FIFO credit
-- lot drawn plus at most one owed_accrual, so the first transfer's amount is NOT
-- the operation's amount — only the sum is. or#891 item 3 compares this against
-- a retry's amount to refuse a reused key carrying a changed body.
-- name: SumLedgerSpendByCoords :one
SELECT COALESCE(SUM(amount), 0)::bigint AS total, count(*)::bigint AS transfers
FROM openrails.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type IN ('credit_spend', 'spend', 'owed_accrual')
  AND operation = sqlc.arg(operation)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text;
