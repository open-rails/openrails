-- #512 double-entry immutable money ledger (ledger_accounts + ledger_transfers).
-- Balances are DERIVED here, never stored. Transfers are append-only.

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
    merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, phase, pending_id,
    source, source_id, grant_id, customer_id, invoker_id, resource, invoice_id
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(debit_account_id)::uuid, sqlc.arg(credit_account_id)::uuid,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(transfer_type)::text, sqlc.arg(phase)::text,
    sqlc.narg(pending_id)::uuid,
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

-- name: IsLedgerPendingResolved :one
SELECT EXISTS (
    SELECT 1 FROM openrails.ledger_transfers
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND pending_id = sqlc.arg(pending_id)::uuid
) AS resolved;

-- LedgerAccountBalance: net credit (credits - debits) over posted+post_pending
-- transfers — the account's derived, spendable balance.
-- name: LedgerAccountBalance :one
SELECT (
    COALESCE(SUM(amount) FILTER (WHERE credit_account_id = sqlc.arg(account_id)::uuid AND phase IN ('posted', 'post_pending')), 0)
    - COALESCE(SUM(amount) FILTER (WHERE debit_account_id = sqlc.arg(account_id)::uuid AND phase IN ('posted', 'post_pending')), 0)
)::bigint AS balance
FROM openrails.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (debit_account_id = sqlc.arg(account_id)::uuid OR credit_account_id = sqlc.arg(account_id)::uuid);

-- LedgerAccountHeld: sum of unresolved pending debits against the account (a
-- two-phase hold reserves balance until it is posted or voided).
-- name: LedgerAccountHeld :one
SELECT COALESCE(SUM(p.amount), 0)::bigint AS held
FROM openrails.ledger_transfers p
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND p.debit_account_id = sqlc.arg(account_id)::uuid
  AND p.phase = 'pending'
  AND NOT EXISTS (
      SELECT 1 FROM openrails.ledger_transfers r WHERE r.pending_id = p.id
  );

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
  AND phase IN ('posted', 'post_pending')
  AND created_at >= sqlc.arg(period_from)::timestamptz
  AND created_at < sqlc.arg(period_to)::timestamptz
GROUP BY transfer_type;

-- LedgerLedgerNet: conservation check — the sum of every account's net balance
-- in a (merchant, currency) ledger. Double-entry guarantees this is 0.
-- name: LedgerLedgerNet :one
SELECT COALESCE(SUM(net), 0)::bigint AS net FROM (
    SELECT
        COALESCE(SUM(t.amount) FILTER (WHERE t.credit_account_id = a.id AND t.phase IN ('posted', 'post_pending')), 0)
        - COALESCE(SUM(t.amount) FILTER (WHERE t.debit_account_id = a.id AND t.phase IN ('posted', 'post_pending')), 0) AS net
    FROM openrails.ledger_accounts a
    LEFT JOIN openrails.ledger_transfers t
        ON t.merchant_id = a.merchant_id AND (t.debit_account_id = a.id OR t.credit_account_id = a.id)
    WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.currency = sqlc.arg(currency)::text
    GROUP BY a.id
) s;
