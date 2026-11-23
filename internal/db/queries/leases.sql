-- A gate's lease and the holds it owns. Every statement names the merchant.

-- name: LockLease :one
SELECT * FROM billing.leases
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: InsertLease :one
-- The first sync under a lease id creates it; a concurrent first sync
-- inserts nothing and locks the row the other made.
INSERT INTO billing.leases (merchant_id, id, created_at, last_sync_at, settled_through_n, receipts)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz,
    sqlc.arg(settled_through_n)::bigint, '[]'::jsonb)
ON CONFLICT (merchant_id, id) DO NOTHING
RETURNING *;

-- name: UpdateLeaseSync :exec
UPDATE billing.leases
SET last_sync_at = sqlc.arg(last_sync_at)::timestamptz, settled_through_n = sqlc.arg(settled_through_n)::bigint,
    receipts = sqlc.arg(receipts)::jsonb
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
INSERT INTO billing.limit_holds (merchant_id, created_at, customer_id, currency, lease_id, limit_set, rate_cards, state, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(created_at)::timestamptz, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text,
    sqlc.arg(lease_id)::uuid, sqlc.arg(limit_set)::text, sqlc.narg(rate_cards)::jsonb, 'open', sqlc.arg(created_at)::timestamptz)
RETURNING *;

-- name: InsertLeaseHoldPart :exec
INSERT INTO billing.limit_hold_parts (merchant_id, hold_id, hold_created_at, customer_id, limit_key, unit, window_starts_at, window_ends_at,
    held, request_units, used_units, answered_units)
VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(hold_id)::uuid, sqlc.arg(hold_created_at)::timestamptz, sqlc.arg(customer_id)::uuid,
    sqlc.arg(limit_key)::text, sqlc.arg(unit)::text, sqlc.arg(window_starts_at)::timestamptz, sqlc.narg(window_ends_at)::timestamptz,
    sqlc.arg(held)::bigint, sqlc.arg(request_units)::bigint, sqlc.arg(used_units)::bigint, sqlc.arg(answered_units)::bigint
);

-- name: ResizeLeaseHoldPart :exec
UPDATE billing.limit_hold_parts
SET held = sqlc.arg(held)::bigint, request_units = sqlc.arg(request_units)::bigint, used_units = sqlc.arg(used_units)::bigint,
    answered_units = sqlc.arg(answered_units)::bigint, contended_at = sqlc.narg(contended_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND hold_id = sqlc.arg(hold_id)::uuid
  AND hold_created_at = sqlc.arg(hold_created_at)::timestamptz AND limit_key = sqlc.arg(limit_key)::text;

-- name: ReleaseLeaseHold :exec
UPDATE billing.limit_holds
SET state = 'released', released_at = sqlc.arg(as_of)::timestamptz, updated_at = sqlc.arg(as_of)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND created_at = sqlc.arg(created_at)::timestamptz AND lease_id IS NOT NULL AND state = 'open';

-- name: SumOtherLeaseHeld :one
-- What other live leases' holds set aside of a limit, in the windows from
-- from_window through to_window and the currency: capacity a want would get
-- if they returned it.
SELECT COALESCE(SUM(p.held), 0)::bigint AS held
FROM billing.limit_hold_parts p
JOIN billing.limit_holds h ON h.merchant_id = p.merchant_id AND h.id = p.hold_id AND h.created_at = p.hold_created_at
JOIN billing.leases l ON l.merchant_id = h.merchant_id AND l.id = h.lease_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.customer_id = sqlc.arg(customer_id)::uuid
  AND p.limit_key = sqlc.arg(limit_key)::text AND h.currency = sqlc.arg(currency)::text
  AND p.window_starts_at >= sqlc.arg(from_window)::timestamptz AND p.window_starts_at <= sqlc.arg(to_window)::timestamptz
  AND p.hold_created_at >= sqlc.arg(held_since)::timestamptz AND h.created_at >= sqlc.arg(held_since)::timestamptz
  AND h.state = 'open' AND h.lease_id <> sqlc.arg(lease_id)::uuid
  AND l.closed_at IS NULL AND l.last_sync_at > sqlc.arg(lease_live_since)::timestamptz;

-- name: MarkLeaseHoldsContended :exec
-- Other live leases' parts holding capacity of a limit a want was refused
-- for shrink on their next sync.
UPDATE billing.limit_hold_parts p
SET contended_at = sqlc.arg(now)::timestamptz
FROM billing.limit_holds h, billing.leases l
WHERE h.merchant_id = p.merchant_id AND h.id = p.hold_id AND h.created_at = p.hold_created_at
  AND l.merchant_id = h.merchant_id AND l.id = h.lease_id
  AND p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.customer_id = sqlc.arg(customer_id)::uuid
  AND p.limit_key = sqlc.arg(limit_key)::text AND h.currency = sqlc.arg(currency)::text
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
