-- The money ledger (modules/money): FIFO blocks + transactions. There is NO
-- balance cache (#491): available is derived from spendable money_blocks and
-- held from active holds + open windows. The per-customer spend mutex is a
-- FOR UPDATE lock on the customers row.
-- amounts are integers in the row currency's minor unit (default µ$ = 1e-6 USD).
-- currency is a system code (USD/USDC/EUR/JPY/SOL); the Go registry is authority.

-- name: LockCustomerForSpend :one
-- The per-customer spend mutex (#491): every spend/hold/capture/deposit/expiry
-- path locks this row FOR UPDATE before reading/mutating the customer's blocks,
-- serializing all money mutations per customer. Returns the customer id.
SELECT id FROM openrails.customers
WHERE id = $1 AND merchant_id = $2
FOR UPDATE;

-- name: SumSpendableMoneyBlocks :one
-- Derived balance (#491): the sum of unexpired, unspent FIFO credit lots — the
-- spendable total for a (merchant, customer, currency). Replaces the cached
-- money_balances.balance.
SELECT COALESCE(SUM(remaining_amount), 0)::bigint
FROM openrails.money_blocks
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND remaining_amount > 0
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now)::timestamptz);

-- name: SumActiveMoneyHeld :one
-- Derived held (#491): active-hold authorizations PLUS open-window unsettled
-- reservations (windows reserve held without a 'hold' row). Replaces the cached
-- money_balances.held_balance.
SELECT (
    COALESCE((SELECT SUM(COALESCE(t.authorized_amount, 0))
              FROM openrails.money_transactions t
              WHERE t.merchant_id = $1 AND t.customer_id = $2 AND t.currency = sqlc.arg(currency)
                AND t.transaction_type = 'hold' AND t.status = 'active'), 0)
  + COALESCE((SELECT SUM(w.held_amount - w.settled_amount)
              FROM openrails.money_windows w
              WHERE w.merchant_id = $1 AND w.customer_id = $2 AND w.currency = sqlc.arg(currency)
                AND w.status = 'open'), 0)
)::bigint;

-- name: ListSpendableMoneyBlocksForUpdate :many
-- FIFO draw order: soonest-expiring first, then oldest.
SELECT * FROM openrails.money_blocks
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND remaining_amount > 0
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now)::timestamptz)
ORDER BY expires_at ASC NULLS LAST, created_at ASC
FOR UPDATE;

-- name: SetMoneyBlockRemaining :exec
UPDATE openrails.money_blocks SET remaining_amount = $2 WHERE id = $1;

-- name: InsertMoneyBlock :exec
INSERT INTO openrails.money_blocks (
    id, merchant_id, customer_id, currency,
    original_amount, remaining_amount, expires_at, source_transaction_id, created_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, $6, $7, $8);

-- name: InsertMoneyTransaction :exec
INSERT INTO openrails.money_transactions (
    id, merchant_id, customer_id, currency, invoker_id, resource, metadata,
    amount, balance_after, transaction_type, status,
    authorized_amount, captured_amount, source, source_id,
    expires_at, description, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18);

-- name: InsertMoneyTransactionIfAbsent :exec
INSERT INTO openrails.money_transactions (
    id, merchant_id, customer_id, currency, invoker_id, resource, metadata,
    amount, balance_after, transaction_type, status,
    authorized_amount, captured_amount, source, source_id,
    expires_at, description, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
ON CONFLICT DO NOTHING;

-- name: GetMoneyTransactionByCoords :one
-- (tenant, payer, currency, transaction_type, source, source_id) is the
-- idempotency key for deposits / withdrawals / holds.
SELECT * FROM openrails.money_transactions
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND transaction_type = $3 AND source = $4 AND source_id = $5
LIMIT 1;

-- name: CountMoneySpendByCoords :one
-- Unified-spend idempotency (#302): a withdrawal OR an owed accrual with these
-- coordinates means the spend already happened.
SELECT count(*) FROM openrails.money_transactions
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND transaction_type IN ('withdrawal', 'owed_accrual')
  AND source = $3 AND source_id = $4;

-- name: GetMoneyTransactionForUpdate :one
SELECT * FROM openrails.money_transactions WHERE id = $1 FOR UPDATE;

-- name: CaptureMoneyHold :exec
UPDATE openrails.money_transactions
SET status = 'captured', captured_amount = $2, amount = $3, balance_after = $4, updated_at = $5
WHERE id = $1;

-- name: ReleaseMoneyHold :exec
UPDATE openrails.money_transactions
SET status = 'released', updated_at = $2
WHERE id = $1;

-- name: ListMoneyTransactionsByPayer :many
SELECT * FROM openrails.money_transactions
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
ORDER BY created_at DESC
LIMIT $3::int OFFSET $4::int;

-- name: CountMoneyTransactionsByPayer :one
SELECT count(*) FROM openrails.money_transactions
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: SumSpentInMoneyWindow :one
-- Spend counted against a rate cap since `since`: settled spend (withdrawals +
-- captured holds) PLUS currently-active holds created in the window, so
-- concurrent in-flight holds can't overshoot a cap. Empty invoker_id = all invokers.
SELECT COALESCE(SUM(
    CASE
        WHEN transaction_type = 'withdrawal' THEN -amount
        WHEN transaction_type = 'hold' AND status = 'captured' THEN -amount
        WHEN transaction_type = 'hold' AND status = 'active' THEN COALESCE(authorized_amount, 0)
        ELSE 0
    END), 0)::bigint
FROM openrails.money_transactions
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND created_at >= sqlc.arg(since)::timestamptz
  AND (sqlc.arg(invoker_id)::text = '' OR invoker_id = sqlc.arg(invoker_id)::text);

-- name: SumActiveMoneyHoldAuthorizations :one
SELECT COALESCE(SUM(COALESCE(authorized_amount, 0)), 0)::bigint
FROM openrails.money_transactions
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND transaction_type = 'hold' AND status = 'active';

-- name: SumMoneyDeposits :one
SELECT COALESCE(SUM(amount), 0)::bigint
FROM openrails.money_transactions
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND transaction_type = 'deposit';

-- name: ListOrphanedExpiredMoneyHolds :many
SELECT * FROM openrails.money_transactions
WHERE merchant_id = $1 AND transaction_type = 'hold' AND status = 'active'
  AND expires_at IS NOT NULL AND expires_at <= sqlc.arg(now)::timestamptz
ORDER BY expires_at ASC;

-- name: SumMoneyMovementsInPeriodByPayer :many
SELECT transaction_type, COALESCE(SUM(amount), 0)::bigint AS total
FROM openrails.money_transactions
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND created_at >= sqlc.arg(period_from)::timestamptz
  AND created_at < sqlc.arg(period_to)::timestamptz
GROUP BY transaction_type;

-- openrails.money_windows: prepaid bulk reservations (#335).

-- name: InsertMoneyWindow :exec
INSERT INTO openrails.money_windows (
    id, merchant_id, customer_id, currency,
    held_amount, settled_amount, status, expires_at, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, $6, $7, $8, $9);

-- name: GetMoneyWindow :one
SELECT * FROM openrails.money_windows WHERE id = $1 LIMIT 1;

-- name: GetMoneyWindowForUpdate :one
SELECT * FROM openrails.money_windows WHERE id = $1 FOR UPDATE;

-- name: AddMoneyWindowSettled :exec
UPDATE openrails.money_windows
SET settled_amount = settled_amount + sqlc.arg(amount)::bigint, updated_at = $2
WHERE id = $1;

-- name: UpdateMoneyWindowReservation :exec
UPDATE openrails.money_windows
SET held_amount = $2, expires_at = $3, updated_at = $4
WHERE id = $1;

-- name: SetMoneyWindowStatus :exec
UPDATE openrails.money_windows
SET status = $2, updated_at = $3
WHERE id = $1;

-- name: ListExpiredOpenMoneyWindowsForUpdate :many
-- Cross-tenant expiry sweep (the window analogue of hold expiry): open windows
-- past expiry, oldest first, SKIP LOCKED so concurrent sweeps don't contend.
SELECT * FROM openrails.money_windows
WHERE status = 'open' AND expires_at <= sqlc.arg(now)::timestamptz
ORDER BY expires_at ASC
LIMIT sqlc.arg(batch_size)::int
FOR UPDATE SKIP LOCKED;

-- name: ListExpiredActiveMoneyHoldsForUpdate :many
-- Hold expiry sweep (cross-tenant, SKIP LOCKED so concurrent sweeps don't contend).
SELECT * FROM openrails.money_transactions
WHERE transaction_type = 'hold' AND status = 'active'
  AND expires_at IS NOT NULL AND expires_at <= sqlc.arg(now)::timestamptz
ORDER BY expires_at ASC
LIMIT sqlc.arg(batch_size)::int
FOR UPDATE SKIP LOCKED;

-- name: ExpireMoneyHold :exec
UPDATE openrails.money_transactions
SET status = 'expired', updated_at = $2
WHERE id = $1;

-- name: DeleteCompactableMoneyBlocks :execrows
-- Credit-block COMPACTION (#491): delete fully-spent (remaining_amount = 0) and
-- expired (past expires_at) lots, keeping the spendable-lot table bounded so the
-- derived available SUM stays cheap. Touches money_blocks ONLY — NEVER the
-- money_transactions receipt/ledger or payments (a deleted block's
-- source_transaction_id reference simply goes away; the deposit receipt
-- survives). Cross-tenant; bounded batch via a CTE of ids; the matching FK from
-- money_blocks -> money_transactions is the only inbound reference and it lives
-- ON the block being deleted, so the delete is self-contained.
WITH doomed AS (
    SELECT id FROM openrails.money_blocks
    WHERE remaining_amount = 0
       OR (expires_at IS NOT NULL AND expires_at <= sqlc.arg(now)::timestamptz)
    ORDER BY id
    LIMIT sqlc.arg(batch_size)::int
)
DELETE FROM openrails.money_blocks WHERE id IN (SELECT id FROM doomed);
