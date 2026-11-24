-- A gate's lease: its usage stream, its in-flight report and the request
-- holds its gate admitted. Every statement names the merchant.

-- name: LockLease :one
SELECT * FROM billing.leases
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: GetLease :one
SELECT * FROM billing.leases
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: InsertLease :one
-- The first sync under a lease id creates it; a concurrent first sync
-- inserts nothing and locks the row the other made.
INSERT INTO billing.leases (merchant_id, id, created_at, last_sync_at, settled_through_n, receipts)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz,
    sqlc.arg(settled_through_n)::bigint, '[]'::jsonb)
ON CONFLICT (merchant_id, id) DO NOTHING
RETURNING *;

-- name: RenewLease :exec
-- A sync makes its lease live again, unless it was closed.
UPDATE billing.leases
SET last_sync_at = GREATEST(last_sync_at, sqlc.arg(now)::timestamptz)
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND closed_at IS NULL;

-- name: SyncElapsed :one
-- How long the sync's transaction has run in real time, in microseconds:
-- the transaction's own clock, which a test's business clock leaves alone.
SELECT (EXTRACT(EPOCH FROM clock_timestamp() - transaction_timestamp()) * 1000000)::bigint AS elapsed_micros;

-- name: UpdateLeaseSettlement :exec
UPDATE billing.leases
SET settled_through_n = sqlc.arg(settled_through_n)::bigint, receipts = sqlc.arg(receipts)::jsonb
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: CloseLease :exec
-- A sync with close ends the lease once; usage against it still settles.
UPDATE billing.leases
SET closed_at = COALESCE(closed_at, GREATEST(created_at, sqlc.arg(closed_at)::timestamptz))
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: CountLiveLeases :one
-- The merchant's leases a sync renewed within the lease lifetime.
SELECT count(*)::int AS live FROM billing.leases
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND closed_at IS NULL
  AND last_sync_at > sqlc.arg(live_since)::timestamptz;

-- name: ListLeaseInFlight :many
SELECT customer_id, limit_key, unit, amount FROM billing.lease_in_flight
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND lease_id = sqlc.arg(lease_id)::uuid
FOR UPDATE;

-- name: UpsertLeaseInFlight :exec
INSERT INTO billing.lease_in_flight (merchant_id, lease_id, customer_id, limit_key, unit, amount, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(lease_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(limit_key)::text,
    sqlc.arg(unit)::text, sqlc.arg(amount)::bigint, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, customer_id, limit_key, unit, lease_id)
DO UPDATE SET amount = EXCLUDED.amount, updated_at = EXCLUDED.updated_at;

-- name: DeleteLeaseInFlight :exec
DELETE FROM billing.lease_in_flight
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND lease_id = sqlc.arg(lease_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid AND limit_key = sqlc.arg(limit_key)::text AND unit = sqlc.arg(unit)::text;

-- name: ClearLeaseInFlight :exec
DELETE FROM billing.lease_in_flight
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND lease_id = sqlc.arg(lease_id)::uuid;

-- name: ListCustomerInFlight :many
-- What live leases other than except_lease report in flight for one
-- customer, by limit and unit.
SELECT f.limit_key, f.unit, SUM(f.amount)::bigint AS amount
FROM billing.lease_in_flight f
JOIN billing.leases l ON l.merchant_id = f.merchant_id AND l.id = f.lease_id
WHERE f.merchant_id = sqlc.arg(merchant_id)::uuid AND f.customer_id = sqlc.arg(customer_id)::uuid
  AND l.closed_at IS NULL AND l.last_sync_at > sqlc.arg(lease_live_since)::timestamptz
  AND (sqlc.narg(except_lease)::uuid IS NULL OR f.lease_id <> sqlc.narg(except_lease)::uuid)
GROUP BY f.limit_key, f.unit;

-- name: ListLeaseOpenHolds :many
-- The open request holds a lease owns, by request id.
SELECT id, created_at, request_id, customer_id FROM billing.limit_holds
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND lease_id = sqlc.arg(lease_id)::uuid AND state = 'open'
  AND created_at >= sqlc.arg(held_since)::timestamptz
  AND (sqlc.narg(request_ids)::text[] IS NULL OR request_id = ANY(sqlc.narg(request_ids)::text[]));

-- name: ListRunningRatesOf :many
-- What many customers' running charges accrue per hour, by currency.
SELECT customer_id, currency, SUM(rate_per_hour)::bigint AS rate_per_hour FROM billing.running_charges
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[]) AND stopped_at IS NULL
GROUP BY customer_id, currency;

