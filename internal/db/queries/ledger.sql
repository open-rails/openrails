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
    allow_debit_negative_up_to,
    source, source_id, grant_id, customer_id, invoker_id, resource, invoice_id
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(debit_account_id)::uuid, sqlc.arg(credit_account_id)::uuid,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(transfer_type)::text,
    sqlc.arg(allow_debit_negative_up_to)::bigint,
    sqlc.narg(source)::text, sqlc.narg(source_id)::text, sqlc.narg(grant_id)::uuid, sqlc.narg(customer_id)::uuid,
    sqlc.narg(invoker_id)::text, sqlc.narg(resource)::text, sqlc.narg(invoice_id)::uuid
)
RETURNING *;

-- name: GetLedgerAccountByID :one
SELECT * FROM openrails.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: GetLedgerTransfer :one
SELECT * FROM openrails.ledger_transfers
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

-- GetLedgerTransferByCoords: idempotency / lookup by the operation coordinate
-- (merchant, customer, currency, transfer_type, source, source_id). Replaces
-- GetMoneyTransactionByCoords. Newest-first so a replay returns the latest row.
-- name: GetLedgerTransferByCoords :one
SELECT * FROM openrails.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type = sqlc.arg(transfer_type)::text
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text
ORDER BY created_at DESC
LIMIT 1;

-- CountLedgerSpendByCoords: unified-spend idempotency — a spend (balance debit)
-- OR an owed accrual with this operation coordinate means it already happened.
-- name: CountLedgerSpendByCoords :one
SELECT count(*) FROM openrails.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type IN ('credit_spend', 'spend', 'owed_accrual')
  AND source = sqlc.arg(source)::text
  AND source_id = sqlc.arg(source_id)::text;

-- GetLedgerSpendByCoords: the first posted spend movement for an idempotent
-- captured request.
-- name: GetLedgerSpendByCoords :one
SELECT * FROM openrails.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND transfer_type IN ('credit_spend', 'spend', 'owed_accrual')
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

-- LedgerLedgerNet: conservation check from maintained counters. Double-entry
-- guarantees this is 0 when the projection is in sync.
-- name: LedgerLedgerNet :one
SELECT COALESCE(SUM(credits_posted - debits_posted), 0)::bigint AS net
FROM openrails.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND currency = sqlc.arg(currency)::text;

-- LedgerAccountCounterDrift: hard-gate invariant for the maintained account
-- counters. Any row returned means ledger_accounts drifted from the immutable
-- transfer log.
-- name: LedgerAccountCounterDrift :many
WITH actual AS (
    SELECT
        a.id,
        COALESCE(SUM(t.amount) FILTER (WHERE t.credit_account_id = a.id), 0)::bigint AS credits_posted,
        COALESCE(SUM(t.amount) FILTER (WHERE t.debit_account_id = a.id), 0)::bigint AS debits_posted
    FROM openrails.ledger_accounts a
    LEFT JOIN openrails.ledger_transfers t
      ON t.merchant_id = a.merchant_id
     AND (t.debit_account_id = a.id OR t.credit_account_id = a.id)
    WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
      AND a.currency = sqlc.arg(currency)::text
    GROUP BY a.id
)
SELECT
    a.id,
    (a.credits_posted - actual.credits_posted)::bigint AS credits_posted_delta,
    (a.debits_posted - actual.debits_posted)::bigint AS debits_posted_delta
FROM openrails.ledger_accounts a
JOIN actual ON actual.id = a.id
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
  AND a.currency = sqlc.arg(currency)::text
  AND (
      a.credits_posted <> actual.credits_posted
      OR a.debits_posted <> actual.debits_posted
  );
