-- billing.running_charges: a customer charged at a rate per hour until the
-- charge is stopped. Every instant is the caller's clock.

-- name: GetRunningChargeByReference :one
SELECT * FROM billing.running_charges
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND reference = sqlc.arg(reference)::text;

-- name: InsertRunningCharge :one
-- No row when the reference is taken: the caller answers the charge it names.
INSERT INTO billing.running_charges (
    merchant_id, reference, customer_id, currency, rate_per_hour, overdraft_amount,
    resource, started_at, posted_through_at, posted_amount, created_at, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(reference)::text, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text,
    sqlc.arg(rate_per_hour)::bigint, sqlc.arg(overdraft_amount)::bigint,
    sqlc.narg(resource)::text, sqlc.arg(started_at)::timestamptz, sqlc.arg(started_at)::timestamptz, 0,
    sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
)
ON CONFLICT (merchant_id, reference) DO NOTHING
RETURNING *;

-- name: PostRunningCharge :one
-- Moves a charge's posting to an instant, no later than its stop.
-- posted_amount is the caller's total through it, which the table checks
-- against running_charge_total. The caller holds the customer lock.
UPDATE billing.running_charges
SET posted_through_at = sqlc.arg(through)::timestamptz,
    posted_amount = sqlc.arg(posted_amount)::bigint,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND posted_through_at <= sqlc.arg(through)::timestamptz
  AND (stopped_at IS NULL OR sqlc.arg(through)::timestamptz <= stopped_at)
RETURNING *;

-- name: StopRunningCharge :one
-- Stops a running charge at an instant not before its last posting; what it
-- accrued since posts later. The caller holds the customer lock.
UPDATE billing.running_charges
SET stopped_at = sqlc.arg(stopped_at)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND stopped_at IS NULL AND posted_through_at <= sqlc.arg(stopped_at)::timestamptz
RETURNING *;

-- name: ListRunningChargesByCustomer :many
-- A customer's running charges in one currency, oldest first.
SELECT * FROM billing.running_charges
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND stopped_at IS NULL
ORDER BY started_at, id;

-- name: ListUnpostedRunningCharges :many
-- A customer's charges in one currency a posting may take, oldest first: the
-- running ones and those stopped after their last posting.
SELECT * FROM billing.running_charges
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND stopped_at IS DISTINCT FROM posted_through_at
ORDER BY started_at, id;

-- name: ListRunningChargesByIDs :many
SELECT * FROM billing.running_charges
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListRunningCharges :many
-- Newest first: ids are uuidv7, so the primary key is the invoice.
SELECT * FROM billing.running_charges
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(running)::boolean IS NULL OR (stopped_at IS NULL) = sqlc.narg(running)::boolean)
  AND (sqlc.narg(after_id)::uuid IS NULL OR id < sqlc.narg(after_id)::uuid)
ORDER BY id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListDepletedRunningCharges :many
-- The running charges of customers whose capacity has run out: the stored
-- run-out instant has passed. Newest first.
SELECT sqlc.embed(c)
FROM billing.balance_watches w
JOIN billing.running_charges c
  ON c.merchant_id = w.merchant_id AND c.customer_id = w.customer_id AND c.currency = w.currency AND c.stopped_at IS NULL
WHERE w.merchant_id = sqlc.arg(merchant_id)::uuid AND w.depletes_at <= sqlc.arg(now)::timestamptz
  AND (sqlc.narg(customer_id)::uuid IS NULL OR w.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(after_id)::uuid IS NULL OR c.id < sqlc.narg(after_id)::uuid)
ORDER BY c.id DESC
LIMIT sqlc.arg(row_limit)::int;
