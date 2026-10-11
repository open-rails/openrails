-- billing.running_charges: a customer charged at a rate per hour until the
-- charge is stopped. billing.balance_watches: when each customer with a running
-- charge is next looked at. Every instant is the caller's clock.

-- name: GetRunningChargeByReference :one
SELECT * FROM billing.running_charges
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND reference = sqlc.arg(reference)::text;

-- name: InsertRunningCharge :one
-- No row when the reference is taken: the caller answers the charge it names.
INSERT INTO billing.running_charges (
    merchant_id, reference, customer_id, currency, rate_per_hour, overdraft_amount, low_balance_amount,
    resource, started_at, posted_through_at, posted_amount, created_at, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(reference)::text, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text,
    sqlc.arg(rate_per_hour)::bigint, sqlc.arg(overdraft_amount)::bigint, sqlc.arg(low_balance_amount)::bigint,
    sqlc.narg(resource)::text, sqlc.arg(started_at)::timestamptz, sqlc.arg(started_at)::timestamptz, 0,
    sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
)
ON CONFLICT (merchant_id, reference) DO NOTHING
RETURNING *;

-- name: PostRunningCharge :one
-- Moves a running charge's posting to an instant, and stops it there when
-- asked. posted_amount is the caller's total through it, which the table
-- checks against running_charge_total. The caller holds the customer lock.
UPDATE billing.running_charges
SET posted_through_at = sqlc.arg(through)::timestamptz,
    posted_amount = sqlc.arg(posted_amount)::bigint,
    stopped_at = CASE WHEN sqlc.arg(stop)::boolean THEN sqlc.arg(through)::timestamptz END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND stopped_at IS NULL AND posted_through_at <= sqlc.arg(through)::timestamptz
RETURNING *;

-- name: ListRunningChargesByCustomer :many
-- A customer's running charges in one currency, oldest first.
SELECT * FROM billing.running_charges
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND stopped_at IS NULL
ORDER BY started_at, id;

-- name: ListRunningCharges :many
-- Newest first: ids are uuidv7, so the primary key is the order.
SELECT * FROM billing.running_charges
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(running)::boolean IS NULL OR (stopped_at IS NULL) = sqlc.narg(running)::boolean)
  AND (sqlc.narg(after_id)::uuid IS NULL OR id < sqlc.narg(after_id)::uuid)
ORDER BY id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: GetRunningChargeTotal :one
-- running_charge_total, for the callers that hold it against the Go rule.
SELECT billing.running_charge_total(sqlc.arg(rate_per_hour)::bigint, sqlc.arg(started_at)::timestamptz, sqlc.arg(at)::timestamptz)::bigint AS total;

-- name: UpsertBalanceWatch :exec
-- What a look found. The notices of a crossing stay until the caller clears them.
INSERT INTO billing.balance_watches (merchant_id, customer_id, currency, next_check_at, depletes_at, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text,
        sqlc.arg(next_check_at)::timestamptz, sqlc.narg(depletes_at)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    next_check_at = EXCLUDED.next_check_at, depletes_at = EXCLUDED.depletes_at, updated_at = EXCLUDED.updated_at;

-- name: DeleteBalanceWatch :exec
DELETE FROM billing.balance_watches
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text;

-- name: GetBalanceWatch :one
SELECT * FROM billing.balance_watches
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text;

-- name: RebuildBalanceWatches :exec
-- A restored merchant's running charges are watched again, due at once.
INSERT INTO billing.balance_watches (merchant_id, customer_id, currency, next_check_at, updated_at)
SELECT DISTINCT rc.merchant_id, rc.customer_id, rc.currency, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
FROM billing.running_charges rc
WHERE rc.merchant_id = sqlc.arg(merchant_id)::uuid AND rc.stopped_at IS NULL
ON CONFLICT (merchant_id, customer_id, currency) DO NOTHING;
