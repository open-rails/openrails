-- A gate's lease and the holds it owns. Every statement names the merchant.

-- name: LockLease :one
SELECT * FROM billing.leases
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: InsertLease :one
-- The first sync under a lease id creates it; a concurrent first sync
-- inserts nothing and locks the row the other made.
INSERT INTO billing.leases (merchant_id, id, created_at, last_sync_at, settled_through_n, receipts, hold_version)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz,
    sqlc.arg(settled_through_n)::bigint, '[]'::jsonb, 0)
ON CONFLICT (merchant_id, id) DO NOTHING
RETURNING *;

-- name: RenewLease :execrows
-- A sync renews its lease only while it is live as the statement runs: the
-- business clock moved on by the transaction's own elapsed time. Readers
-- count a lease live for its lifetime and a grace longer than any sync
-- transaction, so a renewal that commits never revives holds a reader has
-- already counted dead.
UPDATE billing.leases
SET last_sync_at = GREATEST(last_sync_at, sqlc.arg(now)::timestamptz)
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND closed_at IS NULL
  AND last_sync_at > sqlc.arg(live_since)::timestamptz + (clock_timestamp() - transaction_timestamp());

-- name: SyncElapsed :one
-- How long the sync's transaction has run in real time, in microseconds:
-- the transaction's own clock, which a test's business clock leaves alone.
SELECT (EXTRACT(EPOCH FROM clock_timestamp() - transaction_timestamp()) * 1000000)::bigint AS elapsed_micros;

-- name: TryLockCustomerForSpend :one
-- The customer's spend lock unless another transaction holds it: a sync
-- does routine work on the holds of a customer no item names only when it
-- need not wait. No row when skipped.
SELECT id FROM billing.customers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR UPDATE SKIP LOCKED;

-- name: UpdateLeaseSettlement :exec
UPDATE billing.leases
SET settled_through_n = sqlc.arg(settled_through_n)::bigint, receipts = sqlc.arg(receipts)::jsonb,
    hold_version = sqlc.arg(hold_version)::bigint
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: CloseLease :exec
-- A sync with close ends the lease once: it takes no want after, and usage
-- against it still settles.
UPDATE billing.leases
SET closed_at = COALESCE(closed_at, GREATEST(created_at, sqlc.arg(closed_at)::timestamptz))
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListLeaseHolds :many
-- A lease's open holds, retained or not past the hold lifetime, oldest first.
SELECT * FROM billing.limit_holds
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND lease_id = sqlc.arg(lease_id)::uuid
  AND state = 'open' AND created_at >= sqlc.arg(held_since)::timestamptz
ORDER BY created_at, id
FOR UPDATE;

-- name: ListLeaseHoldParts :many
-- What a lease's open holds set aside, by hold and limit.
SELECT p.hold_id, p.hold_created_at, p.customer_id, p.limit_key, p.unit, p.window_starts_at, p.window_ends_at, p.held,
       p.request_units, p.used_units, p.contended_at, p.answered_units
FROM billing.limit_hold_parts p
JOIN billing.limit_holds h ON h.merchant_id = p.merchant_id AND h.id = p.hold_id AND h.created_at = p.hold_created_at
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND h.lease_id = sqlc.arg(lease_id)::uuid AND h.state = 'open'
  AND p.hold_created_at >= sqlc.arg(held_since)::timestamptz AND h.created_at >= sqlc.arg(held_since)::timestamptz
ORDER BY p.hold_created_at, p.hold_id, p.limit_key COLLATE "C";

-- name: InsertLeaseHold :one
INSERT INTO billing.limit_holds (merchant_id, created_at, customer_id, currency, lease_id, limit_set, terms, rate_cards, state, updated_at,
    version, applied_version)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(created_at)::timestamptz, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text,
    sqlc.arg(lease_id)::uuid, sqlc.arg(limit_set)::text, sqlc.arg(terms)::jsonb, sqlc.narg(rate_cards)::jsonb, 'open', sqlc.arg(created_at)::timestamptz,
    sqlc.arg(version)::bigint, sqlc.arg(applied_version)::bigint)
RETURNING *;

-- name: SetLeaseHoldVersions :exec
-- A lease's hold answered a new size, or its gate reported an answer applied.
UPDATE billing.limit_holds
SET version = sqlc.arg(version)::bigint, applied_version = sqlc.arg(applied_version)::bigint, updated_at = sqlc.arg(as_of)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND created_at = sqlc.arg(created_at)::timestamptz AND lease_id IS NOT NULL;

-- name: InsertLeaseHoldPart :exec
INSERT INTO billing.limit_hold_parts (merchant_id, hold_id, hold_created_at, customer_id, limit_key, unit, window_starts_at, window_ends_at,
    held, request_units, used_units, answered_units)
VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(hold_id)::uuid, sqlc.arg(hold_created_at)::timestamptz, sqlc.arg(customer_id)::uuid,
    sqlc.arg(limit_key)::text, sqlc.arg(unit)::text, sqlc.arg(window_starts_at)::timestamptz, sqlc.narg(window_ends_at)::timestamptz,
    sqlc.arg(held)::bigint, sqlc.arg(request_units)::bigint, sqlc.arg(used_units)::bigint, sqlc.arg(answered_units)::bigint
);

-- name: ResizeLeaseHoldPart :exec
-- A sync writes its own parts only under their customer's lock, and never
-- contended_at: another lease's refusal marks it under the same lock.
UPDATE billing.limit_hold_parts
SET held = sqlc.arg(held)::bigint, request_units = sqlc.arg(request_units)::bigint, used_units = sqlc.arg(used_units)::bigint,
    answered_units = sqlc.arg(answered_units)::bigint
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND hold_id = sqlc.arg(hold_id)::uuid
  AND hold_created_at = sqlc.arg(hold_created_at)::timestamptz AND limit_key = sqlc.arg(limit_key)::text;

-- name: ReleaseLeaseHold :exec
UPDATE billing.limit_holds
SET state = 'released', released_at = sqlc.arg(as_of)::timestamptz, updated_at = sqlc.arg(as_of)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND created_at = sqlc.arg(created_at)::timestamptz AND lease_id IS NOT NULL AND state = 'open';

-- name: SumOtherLeaseHeld :one
-- What other live leases' holds set aside of a limit, in the windows from
-- from_window through to_window and, when named, the currency (balance's):
-- capacity a want or an admission would get if they returned it.
SELECT COALESCE(SUM(p.held), 0)::bigint AS held
FROM billing.limit_hold_parts p
JOIN billing.limit_holds h ON h.merchant_id = p.merchant_id AND h.id = p.hold_id AND h.created_at = p.hold_created_at
JOIN billing.leases l ON l.merchant_id = h.merchant_id AND l.id = h.lease_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.customer_id = sqlc.arg(customer_id)::uuid
  AND p.limit_key = sqlc.arg(limit_key)::text
  AND (sqlc.narg(currency)::text IS NULL OR h.currency = sqlc.narg(currency)::text)
  AND p.window_starts_at >= sqlc.arg(from_window)::timestamptz AND p.window_starts_at <= sqlc.arg(to_window)::timestamptz
  AND p.hold_created_at >= sqlc.arg(held_since)::timestamptz AND h.created_at >= sqlc.arg(held_since)::timestamptz
  AND h.state = 'open' AND h.lease_id <> sqlc.arg(lease_id)::uuid
  AND l.closed_at IS NULL AND l.last_sync_at > sqlc.arg(lease_live_since)::timestamptz;

-- name: MarkLeaseHoldsContended :exec
-- Other live leases' parts holding capacity of a limit a want or an
-- admission was refused capacity_held for shrink on their next sync. The
-- caller holds the customer's lock.
UPDATE billing.limit_hold_parts p
SET contended_at = sqlc.arg(now)::timestamptz
FROM billing.limit_holds h, billing.leases l
WHERE h.merchant_id = p.merchant_id AND h.id = p.hold_id AND h.created_at = p.hold_created_at
  AND l.merchant_id = h.merchant_id AND l.id = h.lease_id
  AND p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.customer_id = sqlc.arg(customer_id)::uuid
  AND p.limit_key = sqlc.arg(limit_key)::text
  AND (sqlc.narg(currency)::text IS NULL OR h.currency = sqlc.narg(currency)::text)
  AND p.window_starts_at >= sqlc.arg(from_window)::timestamptz AND p.window_starts_at <= sqlc.arg(to_window)::timestamptz
  AND p.hold_created_at >= sqlc.arg(held_since)::timestamptz AND h.created_at >= sqlc.arg(held_since)::timestamptz
  AND p.held > 0 AND h.state = 'open' AND h.lease_id <> sqlc.arg(lease_id)::uuid
  AND l.closed_at IS NULL AND l.last_sync_at > sqlc.arg(lease_live_since)::timestamptz;

-- name: ConsumeLeaseHoldPart :exec
-- Usage a live lease decided against its hold leaves the hold.
UPDATE billing.limit_hold_parts
SET held = GREATEST(held - sqlc.arg(units)::bigint, 0), used_units = COALESCE(used_units, 0) + sqlc.arg(units)::bigint,
    answered_units = GREATEST(COALESCE(answered_units, held) - sqlc.arg(units)::bigint, 0)
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND hold_id = sqlc.arg(hold_id)::uuid
  AND hold_created_at = sqlc.arg(hold_created_at)::timestamptz AND limit_key = sqlc.arg(limit_key)::text;

-- name: ListCustomersMovedSince :many
-- Of the customers a lease holds for, those whose limits settled or whose
-- usage accrued since an instant: their idle holds are sized again.
SELECT w.customer_id FROM billing.limit_windows w
WHERE w.merchant_id = sqlc.arg(merchant_id)::uuid AND w.customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
  AND w.updated_at > sqlc.arg(since)::timestamptz
UNION
SELECT a.customer_id FROM billing.usage_accruals a
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
  AND a.day_at >= sqlc.arg(accrued_since)::timestamptz AND a.updated_at > sqlc.arg(since)::timestamptz;
