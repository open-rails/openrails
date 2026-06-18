-- modules/money serialization + prepaid windows. After the #512 hard cut the
-- single-entry money_blocks / money_transactions tables are gone: balance is
-- derived from the #512 ledger (internal/db/queries/ledger.sql), credit lots are
-- #514 grants (grants.sql), and the per-customer spend mutex is a FOR UPDATE
-- lock on the customers row. Held is the durable open-window unsettled
-- reservation (#335). amounts are integers in the row currency's precision.

-- name: LockCustomerForSpend :one
-- The per-customer spend mutex (#491): every spend/hold/capture/deposit/expiry
-- path locks this row FOR UPDATE before reading/mutating the customer's ledger,
-- serializing all money mutations per customer. Returns the customer id.
SELECT id FROM openrails.customers
WHERE id = $1 AND merchant_id = $2
FOR UPDATE;

-- name: SumActiveMoneyHeld :one
-- Derived held (#505): durable open-window unsettled reservations only.
-- Request/admit holds live in Redis and are included by the admission layer.
SELECT COALESCE((SELECT SUM(w.held_amount - w.settled_amount)
              FROM openrails.money_windows w
              WHERE w.merchant_id = $1 AND w.customer_id = $2 AND w.currency = sqlc.arg(currency)
                AND w.status = 'open'), 0)::bigint;

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
