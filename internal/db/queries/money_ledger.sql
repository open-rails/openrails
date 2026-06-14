-- The money ledger (modules/money): balances, FIFO blocks, transactions.
-- amounts are integers in the row currency's minor unit (default µ$ = 1e-6 USD).
-- currency is a system code (USD/USDC/EUR/JPY/SOL); the Go registry is authority.

-- name: GetMoneyBalance :one
SELECT * FROM openrails.money_balances
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
LIMIT 1;

-- name: LockMoneyBalance :one
SELECT * FROM openrails.money_balances
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
FOR UPDATE;

-- name: InsertMoneyBalanceIfAbsent :exec
-- First-touch materialization; ON CONFLICT DO NOTHING targets the
-- (tenant, payer, currency) uniqueness so concurrent first-touch is safe.
INSERT INTO openrails.money_balances (
    id, merchant_id, customer_id, currency, balance, held_balance, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), 0, 0, sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (merchant_id, customer_id, currency) DO NOTHING;

-- name: SetMoneyBalance :exec
UPDATE openrails.money_balances
SET balance = $3, updated_at = $4
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: SetMoneyHeldBalance :exec
UPDATE openrails.money_balances
SET held_balance = $3, updated_at = $4
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

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
    id, merchant_id, customer_id, currency, actor, resource, metadata,
    amount, balance_after, transaction_type, status,
    authorized_amount, captured_amount, source, source_id,
    expires_at, description, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18);

-- name: InsertMoneyTransactionIfAbsent :exec
INSERT INTO openrails.money_transactions (
    id, merchant_id, customer_id, currency, actor, resource, metadata,
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
-- concurrent in-flight holds can't overshoot a cap. Empty actor = all actors.
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
  AND (sqlc.arg(actor)::text = '' OR actor = sqlc.arg(actor)::text);

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

-- name: ListMoneyHeldBalanceDrift :many
-- Balance rows whose stored held_balance disagrees with the sum of their
-- currently-active holds (reconciliation, alert-only).
SELECT b.merchant_id, b.customer_id, b.currency,
       b.held_balance AS stored,
       COALESCE((SELECT SUM(COALESCE(t.authorized_amount, 0))
                 FROM openrails.money_transactions t
                 WHERE t.merchant_id = b.merchant_id
                   AND t.customer_id = b.customer_id
                   AND t.currency = b.currency
                   AND t.transaction_type = 'hold' AND t.status = 'active'), 0)::bigint AS computed
FROM openrails.money_balances b
WHERE b.merchant_id = $1
  AND b.held_balance <> COALESCE((SELECT SUM(COALESCE(t.authorized_amount, 0))
                 FROM openrails.money_transactions t
                 WHERE t.merchant_id = b.merchant_id
                   AND t.customer_id = b.customer_id
                   AND t.currency = b.currency
                   AND t.transaction_type = 'hold' AND t.status = 'active'), 0);

-- name: ListMoneyBalanceAnomalies :many
SELECT b.merchant_id, b.customer_id, b.currency, b.balance, b.held_balance
FROM openrails.money_balances b
WHERE b.merchant_id = $1
  AND (b.balance < 0 OR b.held_balance < 0 OR b.held_balance > b.balance);

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

-- name: ListExpiredMoneyBlocksForUpdate :many
-- Money block expiry sweep (cross-tenant, SKIP LOCKED).
SELECT * FROM openrails.money_blocks
WHERE remaining_amount > 0
  AND expires_at IS NOT NULL AND expires_at <= sqlc.arg(now)::timestamptz
ORDER BY expires_at ASC
LIMIT sqlc.arg(batch_size)::int
FOR UPDATE SKIP LOCKED;
