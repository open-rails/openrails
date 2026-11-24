-- billing.limit_holds and billing.limit_hold_parts are partitioned by month on
-- the hold's creation. A lookup by request id names the retained range;
-- everything after it names the hold's own created_at, which is one partition.

-- Serializes admission per request id: the partitioned key carries
-- created_at, so it cannot refuse the same id admitted twice.
-- name: LockHoldRequest :exec
SELECT pg_advisory_xact_lock(hashtextextended('openrails.admission_request:' || sqlc.arg(merchant_id)::uuid::text || ':' || sqlc.arg(request_id)::text, 0));

-- name: GetRequestHold :one
SELECT * FROM billing.limit_holds
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text
  AND created_at >= sqlc.arg(created_since)::timestamptz
ORDER BY created_at DESC
LIMIT 1;

-- name: LockHold :one
SELECT * FROM billing.limit_holds
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND created_at = sqlc.arg(created_at)::timestamptz
FOR UPDATE;

-- name: InsertRequestHold :one
INSERT INTO billing.limit_holds (
    merchant_id, created_at, customer_id, currency, lease_id, request_id, estimated_amount, available_amount, terms,
    estimate_usage, rate_cards, requested_expires_at, expires_at, state, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(created_at)::timestamptz, sqlc.arg(customer_id)::uuid,
    sqlc.arg(currency)::text, sqlc.narg(lease_id)::uuid, sqlc.arg(request_id)::text, sqlc.arg(estimated_amount)::bigint, sqlc.arg(available_amount)::bigint,
    sqlc.arg(terms)::jsonb, sqlc.narg(estimate_usage)::jsonb, sqlc.narg(rate_cards)::jsonb,
    sqlc.narg(requested_expires_at)::timestamptz, sqlc.narg(requested_expires_at)::timestamptz, 'open', sqlc.arg(created_at)::timestamptz
)
RETURNING *;

-- name: InsertHoldPart :exec
INSERT INTO billing.limit_hold_parts (merchant_id, hold_id, hold_created_at, customer_id, limit_key, unit, window_starts_at, window_ends_at, held)
VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(hold_id)::uuid, sqlc.arg(hold_created_at)::timestamptz, sqlc.arg(customer_id)::uuid,
    sqlc.arg(limit_key)::text, sqlc.arg(unit)::text, sqlc.arg(window_starts_at)::timestamptz, sqlc.narg(window_ends_at)::timestamptz,
    sqlc.arg(held)::bigint
);

-- What one hold set aside of each limit, by limit.
-- name: ListHoldParts :many
SELECT limit_key, unit, window_starts_at, window_ends_at, held
FROM billing.limit_hold_parts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND hold_id = sqlc.arg(hold_id)::uuid
  AND hold_created_at = sqlc.arg(hold_created_at)::timestamptz
ORDER BY limit_key COLLATE "C";

-- The units live holds set aside in a limit's windows from from_window
-- through to_window (one window, or a rolling limit's buckets), and, for the
-- current window (in_flight), what live leases other than except_lease
-- report in flight. A request's hold is live until its deadline; past it, it
-- still counts where counts_expired (a window with an end) until it is
-- captured or released. A gate's hold is live while its lease is, and for a
-- lease lifetime after it is made: one its lapsed lease made counts at once.
-- name: SumWindowHeld :one
SELECT (COALESCE((SELECT SUM(p.held)
    FROM billing.limit_hold_parts p
    JOIN billing.limit_holds h ON h.merchant_id = p.merchant_id AND h.id = p.hold_id AND h.created_at = p.hold_created_at
    WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.customer_id = sqlc.arg(customer_id)::uuid
      AND p.limit_key = sqlc.arg(limit_key)::text
      AND p.window_starts_at >= sqlc.arg(from_window)::timestamptz AND p.window_starts_at <= sqlc.arg(to_window)::timestamptz
      AND p.hold_created_at >= sqlc.arg(held_since)::timestamptz AND h.created_at >= sqlc.arg(held_since)::timestamptz
      AND h.state = 'open'
      AND CASE WHEN h.lease_id IS NULL
          THEN sqlc.arg(counts_expired)::boolean OR h.expires_at > sqlc.arg(as_of)::timestamptz
          ELSE h.created_at > sqlc.arg(lease_live_since)::timestamptz OR EXISTS (SELECT 1 FROM billing.leases l WHERE l.merchant_id = h.merchant_id AND l.id = h.lease_id
              AND l.closed_at IS NULL AND l.last_sync_at > sqlc.arg(lease_live_since)::timestamptz) END), 0)
  + CASE WHEN sqlc.arg(in_flight)::boolean THEN COALESCE((SELECT SUM(f.amount)
    FROM billing.lease_in_flight f
    JOIN billing.leases l ON l.merchant_id = f.merchant_id AND l.id = f.lease_id
    WHERE f.merchant_id = sqlc.arg(merchant_id)::uuid AND f.customer_id = sqlc.arg(customer_id)::uuid
      AND f.limit_key = sqlc.arg(limit_key)::text
      AND l.closed_at IS NULL AND l.last_sync_at > sqlc.arg(lease_live_since)::timestamptz
      AND (sqlc.narg(except_lease)::uuid IS NULL OR f.lease_id <> sqlc.narg(except_lease)::uuid)), 0) ELSE 0 END)::bigint AS held;

-- name: GetFinancialHeldAmount :one
-- The one financial hold total in PostgreSQL: live holds' balance parts,
-- what gates have in flight, pending refunds, and what the customer's
-- running charges have accrued and not posted (accrued, also answered
-- apart); unposted usage is the event store's, added by the caller. GetAdmissionCapacity computes the same sum.
-- A live hold was created within the hold lifetime, which is held_since. A
-- posting through an instant holds the accrual as of it (accrued_as_of);
-- every other read, as of as_of.
SELECT (h.claims + h.accrued)::bigint AS held, h.accrued::bigint AS accrued
FROM (SELECT (COALESCE((SELECT SUM(hp.held)
              FROM billing.limit_hold_parts hp
              JOIN billing.limit_holds lh ON lh.merchant_id = hp.merchant_id AND lh.id = hp.hold_id AND lh.created_at = hp.hold_created_at
             WHERE hp.merchant_id = sqlc.arg(merchant_id)::uuid AND hp.customer_id = sqlc.arg(customer_id)::uuid AND hp.limit_key = 'balance'
               AND lh.currency = sqlc.arg(currency)::text AND lh.state = 'open'
               AND hp.hold_created_at >= sqlc.arg(held_since)::timestamptz AND lh.created_at >= sqlc.arg(held_since)::timestamptz
               AND CASE WHEN lh.lease_id IS NULL THEN lh.expires_at > sqlc.arg(as_of)::timestamptz
                   ELSE lh.created_at > sqlc.arg(lease_live_since)::timestamptz OR EXISTS (SELECT 1 FROM billing.leases l WHERE l.merchant_id = lh.merchant_id AND l.id = lh.lease_id
                       AND l.closed_at IS NULL AND l.last_sync_at > sqlc.arg(lease_live_since)::timestamptz) END), 0)
     -- What live gates report in flight of the balance.
     + COALESCE((SELECT SUM(f.amount)
              FROM billing.lease_in_flight f
              JOIN billing.leases l ON l.merchant_id = f.merchant_id AND l.id = f.lease_id
             WHERE f.merchant_id = sqlc.arg(merchant_id)::uuid AND f.customer_id = sqlc.arg(customer_id)::uuid
               AND f.limit_key = 'balance' AND f.unit = sqlc.arg(currency)::text
               AND l.closed_at IS NULL AND l.last_sync_at > sqlc.arg(lease_live_since)::timestamptz), 0)
     -- What pending refunds reserve of a purchased lot, up to its units left.
     -- Retired credit funds its refund from the retired account; an unswept
     -- lapsed lot is entirely unavailable.
     + COALESCE((SELECT sum(CASE WHEN g.expires_at <= sqlc.arg(as_of)::timestamptz
         THEN g.units_credited - g.units_debited ELSE LEAST(g.units_reserved, g.units_credited - g.units_debited) END)
       FROM billing.credit_grants g
       WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.customer_id = sqlc.arg(customer_id)::uuid AND g.unit = sqlc.arg(currency)::text
         AND g.units_reserved > 0 AND g.units_credited > g.units_debited), 0)) AS claims,
     COALESCE((SELECT SUM(GREATEST(billing.running_charge_total(rc.rate_per_hour, rc.started_at, COALESCE(sqlc.narg(accrued_as_of)::timestamptz, sqlc.arg(as_of)::timestamptz)) - rc.posted_amount, 0))
              FROM billing.running_charges rc
             WHERE rc.merchant_id = sqlc.arg(merchant_id)::uuid AND rc.customer_id = sqlc.arg(customer_id)::uuid AND rc.currency = sqlc.arg(currency)::text AND rc.stopped_at IS NULL), 0) AS accrued) h;

-- name: RequestHoldCaptureTermsMatch :one
SELECT capture_terms = sqlc.arg(capture_terms)::jsonb AS matches
FROM billing.limit_holds
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND created_at = sqlc.arg(created_at)::timestamptz;

-- name: CaptureRequestHold :one
UPDATE billing.limit_holds
SET state = 'captured', capture_terms = sqlc.arg(capture_terms)::jsonb, captured_amount = sqlc.arg(amount)::bigint,
    captured_charges = sqlc.arg(captured_charges)::jsonb, captured_at = sqlc.arg(as_of)::timestamptz, updated_at = sqlc.arg(as_of)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND created_at = sqlc.arg(created_at)::timestamptz AND request_id IS NOT NULL
  AND state <> 'captured'
RETURNING *;

-- name: ReleaseRequestHold :execrows
UPDATE billing.limit_holds
SET state = 'released', released_at = sqlc.arg(as_of)::timestamptz, updated_at = sqlc.arg(as_of)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND created_at = sqlc.arg(created_at)::timestamptz AND request_id IS NOT NULL AND state = 'open';

-- name: ExtendRequestHold :execrows
UPDATE billing.limit_holds SET expires_at = sqlc.arg(expires_at)::timestamptz, updated_at = sqlc.arg(as_of)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND created_at = sqlc.arg(created_at)::timestamptz AND request_id IS NOT NULL AND state = 'open'
  AND (expires_at IS NULL OR expires_at > sqlc.arg(as_of)::timestamptz)
  AND (expires_at IS NULL OR expires_at <= sqlc.arg(expires_at)::timestamptz);
