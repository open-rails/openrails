-- The credit ledger (modules/credits): balances, FIFO blocks, transactions.

-- name: GetCreditBalance :one
SELECT * FROM openrails.credit_balances
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
LIMIT 1;

-- name: LockCreditBalance :one
SELECT * FROM openrails.credit_balances
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
FOR UPDATE;

-- name: InsertCreditBalanceIfAbsent :exec
-- First-touch materialization; ON CONFLICT DO NOTHING targets the
-- (tenant, payer, credit_type) uniqueness so concurrent first-touch is safe.
INSERT INTO openrails.credit_balances (
    id, tenant_id, tenant_subject_id, credit_type_id, balance, held_balance, created_at, updated_at
) VALUES ($1, $2, $3, $4, 0, 0, sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (tenant_id, tenant_subject_id, credit_type_id) DO NOTHING;

-- name: SetCreditBalance :exec
UPDATE openrails.credit_balances
SET balance = $4, updated_at = $5
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3;

-- name: SetCreditHeldBalance :exec
UPDATE openrails.credit_balances
SET held_balance = $4, updated_at = $5
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3;

-- name: ListSpendableCreditBlocksForUpdate :many
-- FIFO draw order: soonest-expiring first, then oldest.
SELECT * FROM openrails.credit_blocks
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
  AND remaining_amount > 0
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now)::timestamptz)
ORDER BY expires_at ASC NULLS LAST, created_at ASC
FOR UPDATE;

-- name: SetCreditBlockRemaining :exec
UPDATE openrails.credit_blocks SET remaining_amount = $2 WHERE id = $1;

-- name: InsertCreditBlock :exec
INSERT INTO openrails.credit_blocks (
    id, tenant_id, tenant_subject_id, credit_type_id,
    original_amount, remaining_amount, expires_at, source_transaction_id, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: InsertCreditTransaction :exec
INSERT INTO openrails.credit_transactions (
    id, tenant_id, tenant_subject_id, actor, resource, metadata, credit_type_id,
    amount, balance_after, transaction_type, status,
    authorized_amount, captured_amount, source, source_id,
    expires_at, description, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19);

-- name: InsertCreditTransactionIfAbsent :exec
INSERT INTO openrails.credit_transactions (
    id, tenant_id, tenant_subject_id, actor, resource, metadata, credit_type_id,
    amount, balance_after, transaction_type, status,
    authorized_amount, captured_amount, source, source_id,
    expires_at, description, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
ON CONFLICT DO NOTHING;

-- name: GetCreditTransactionByCoords :one
-- (tenant, payer, credit_type, transaction_type, source, source_id) is the
-- idempotency key for deposits / withdrawals / holds.
SELECT * FROM openrails.credit_transactions
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
  AND transaction_type = $4 AND source = $5 AND source_id = $6
LIMIT 1;

-- name: CountCreditSpendByCoords :one
-- Unified-spend idempotency (#302): a withdrawal OR an owed accrual with these
-- coordinates means the spend already happened.
SELECT count(*) FROM openrails.credit_transactions
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
  AND transaction_type IN ('withdrawal', 'owed_accrual')
  AND source = $4 AND source_id = $5;

-- name: GetCreditTransactionForUpdate :one
SELECT * FROM openrails.credit_transactions WHERE id = $1 FOR UPDATE;

-- name: CaptureCreditHold :exec
UPDATE openrails.credit_transactions
SET status = 'captured', captured_amount = $2, amount = $3, balance_after = $4, updated_at = $5
WHERE id = $1;

-- name: ReleaseCreditHold :exec
UPDATE openrails.credit_transactions
SET status = 'released', updated_at = $2
WHERE id = $1;

-- name: ListCreditTransactionsByPayer :many
SELECT * FROM openrails.credit_transactions
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
ORDER BY created_at DESC
LIMIT $4::int OFFSET $5::int;

-- name: CountCreditTransactionsByPayer :one
SELECT count(*) FROM openrails.credit_transactions
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3;

-- name: SumSpentInWindow :one
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
FROM openrails.credit_transactions
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
  AND created_at >= sqlc.arg(since)::timestamptz
  AND (sqlc.arg(actor)::text = '' OR actor = sqlc.arg(actor)::text);

-- name: SumActiveHoldAuthorizations :one
SELECT COALESCE(SUM(COALESCE(authorized_amount, 0)), 0)::bigint
FROM openrails.credit_transactions
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
  AND transaction_type = 'hold' AND status = 'active';

-- name: SumCreditDeposits :one
SELECT COALESCE(SUM(amount), 0)::bigint
FROM openrails.credit_transactions
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
  AND transaction_type = 'deposit';

-- name: ListOrphanedExpiredHolds :many
SELECT * FROM openrails.credit_transactions
WHERE tenant_id = $1 AND transaction_type = 'hold' AND status = 'active'
  AND expires_at IS NOT NULL AND expires_at <= sqlc.arg(now)::timestamptz
ORDER BY expires_at ASC;

-- name: SumMoneyMovementsInPeriod :many
SELECT transaction_type, COALESCE(SUM(amount), 0)::bigint AS total
FROM openrails.credit_transactions
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
  AND created_at >= sqlc.arg(period_from)::timestamptz
  AND created_at < sqlc.arg(period_to)::timestamptz
GROUP BY transaction_type;

-- name: ListHeldBalanceDrift :many
-- Balance rows whose stored held_balance disagrees with the sum of their
-- currently-active holds (reconciliation, alert-only).
SELECT b.tenant_id, b.tenant_subject_id, b.credit_type_id,
       b.held_balance AS stored,
       COALESCE((SELECT SUM(COALESCE(t.authorized_amount, 0))
                 FROM openrails.credit_transactions t
                 WHERE t.tenant_id = b.tenant_id
                   AND t.tenant_subject_id = b.tenant_subject_id
                   AND t.credit_type_id = b.credit_type_id
                   AND t.transaction_type = 'hold' AND t.status = 'active'), 0)::bigint AS computed
FROM openrails.credit_balances b
WHERE b.tenant_id = $1
  AND b.held_balance <> COALESCE((SELECT SUM(COALESCE(t.authorized_amount, 0))
                 FROM openrails.credit_transactions t
                 WHERE t.tenant_id = b.tenant_id
                   AND t.tenant_subject_id = b.tenant_subject_id
                   AND t.credit_type_id = b.credit_type_id
                   AND t.transaction_type = 'hold' AND t.status = 'active'), 0);

-- name: ListBalanceAnomalies :many
SELECT b.tenant_id, b.tenant_subject_id, b.credit_type_id, b.balance, b.held_balance
FROM openrails.credit_balances b
WHERE b.tenant_id = $1
  AND (b.balance < 0 OR b.held_balance < 0 OR b.held_balance > b.balance);

-- openrails.credit_windows: prepaid bulk reservations (#335).

-- name: InsertCreditWindow :exec
INSERT INTO openrails.credit_windows (
    id, tenant_id, tenant_subject_id, credit_type_id,
    held_amount, settled_amount, status, expires_at, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: GetCreditWindow :one
SELECT * FROM openrails.credit_windows WHERE id = $1 LIMIT 1;

-- name: GetCreditWindowForUpdate :one
SELECT * FROM openrails.credit_windows WHERE id = $1 FOR UPDATE;

-- name: AddCreditWindowSettled :exec
UPDATE openrails.credit_windows
SET settled_amount = settled_amount + sqlc.arg(amount)::bigint, updated_at = $2
WHERE id = $1;

-- name: UpdateCreditWindowReservation :exec
UPDATE openrails.credit_windows
SET held_amount = $2, expires_at = $3, updated_at = $4
WHERE id = $1;

-- name: SetCreditWindowStatus :exec
UPDATE openrails.credit_windows
SET status = $2, updated_at = $3
WHERE id = $1;

-- name: ListExpiredOpenCreditWindowsForUpdate :many
-- Cross-tenant expiry sweep (the window analogue of hold expiry): open windows
-- past expiry, oldest first, SKIP LOCKED so concurrent sweeps don't contend.
SELECT * FROM openrails.credit_windows
WHERE status = 'open' AND expires_at <= sqlc.arg(now)::timestamptz
ORDER BY expires_at ASC
LIMIT sqlc.arg(batch_size)::int
FOR UPDATE SKIP LOCKED;

-- name: ListExpiredActiveHoldsForUpdate :many
-- Hold expiry sweep (cross-tenant, SKIP LOCKED so concurrent sweeps don't contend).
SELECT * FROM openrails.credit_transactions
WHERE transaction_type = 'hold' AND status = 'active'
  AND expires_at IS NOT NULL AND expires_at <= sqlc.arg(now)::timestamptz
ORDER BY expires_at ASC
LIMIT sqlc.arg(batch_size)::int
FOR UPDATE SKIP LOCKED;

-- name: ExpireCreditHold :exec
UPDATE openrails.credit_transactions
SET status = 'expired', updated_at = $2
WHERE id = $1;

-- name: ListExpiredCreditBlocksForUpdate :many
-- Credit block expiry sweep (cross-tenant, SKIP LOCKED).
SELECT * FROM openrails.credit_blocks
WHERE remaining_amount > 0
  AND expires_at IS NOT NULL AND expires_at <= sqlc.arg(now)::timestamptz
ORDER BY expires_at ASC
LIMIT sqlc.arg(batch_size)::int
FOR UPDATE SKIP LOCKED;

-- name: ListActiveCreditTypesWithBalance :many
-- GetMyCredits: every active credit type with the payer's balance (NULL when
-- the payer has never touched that type). NOTE: the bun-era handler joined on
-- a nonexistent ucb.user_id column and errored at runtime; the join key is
-- the payer's tenant_subject_id.
SELECT ct.id AS credit_type_id, ct.name, ct.display_name, ct.unit, ct.decimal_places,
       ucb.balance, ucb.held_balance
FROM openrails.credit_types ct
LEFT JOIN openrails.credit_balances ucb
  ON ucb.credit_type_id = ct.id
 AND ucb.tenant_subject_id = sqlc.arg(tenant_subject_id)::uuid
WHERE ct.is_active = true;
