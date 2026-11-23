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
    merchant_id, created_at, customer_id, currency, request_id, estimated_amount, available_amount, terms,
    estimate_usage, rate_cards, requested_expires_at, expires_at, state, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(created_at)::timestamptz, sqlc.arg(customer_id)::uuid,
    sqlc.arg(currency)::text, sqlc.arg(request_id)::text, sqlc.arg(estimated_amount)::bigint, sqlc.arg(available_amount)::bigint,
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
-- through to_window (one window, or a rolling limit's buckets). A request's
-- hold is live until its deadline; past it, it still counts where
-- counts_expired (a window with an end) until it is captured or released. A
-- lease's hold is live while its lease is.
-- name: SumWindowHeld :one
SELECT COALESCE(SUM(p.held), 0)::bigint AS held
FROM billing.limit_hold_parts p
JOIN billing.limit_holds h ON h.merchant_id = p.merchant_id AND h.id = p.hold_id AND h.created_at = p.hold_created_at
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.customer_id = sqlc.arg(customer_id)::uuid
  AND p.limit_key = sqlc.arg(limit_key)::text
  AND p.window_starts_at >= sqlc.arg(from_window)::timestamptz AND p.window_starts_at <= sqlc.arg(to_window)::timestamptz
  AND p.hold_created_at >= sqlc.arg(held_since)::timestamptz AND h.created_at >= sqlc.arg(held_since)::timestamptz
  AND h.state = 'open'
  AND CASE WHEN h.request_id IS NOT NULL
      THEN sqlc.arg(counts_expired)::boolean OR h.expires_at > sqlc.arg(as_of)::timestamptz
      ELSE EXISTS (SELECT 1 FROM billing.leases l WHERE l.merchant_id = h.merchant_id AND l.id = h.lease_id
          AND l.closed_at IS NULL AND l.last_sync_at > sqlc.arg(lease_live_since)::timestamptz) END;

-- name: GetFinancialHeldAmount :one
-- The one financial hold total: live holds' balance parts, pending refunds,
-- unposted usage accruals, and what the customer's running charges have
-- accrued and not posted
-- (accrued, also answered apart). GetAdmissionCapacity computes the same sum.
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
               AND CASE WHEN lh.request_id IS NOT NULL THEN lh.expires_at > sqlc.arg(as_of)::timestamptz
                   ELSE EXISTS (SELECT 1 FROM billing.leases l WHERE l.merchant_id = lh.merchant_id AND l.id = lh.lease_id
                       AND l.closed_at IS NULL AND l.last_sync_at > sqlc.arg(lease_live_since)::timestamptz) END), 0)
     -- Retired credit funds its refund from the retired account. Only value
     -- still in customer_balance remains reserved here; an unswept expired
     -- source is entirely unavailable, including its unrefunded remainder.
     -- A refund returns the credit line's money first, so a pending refund
     -- holds its share of the line's paid amount, not of the whole payment.
     + COALESCE((SELECT sum(CASE WHEN g.ends_at <= sqlc.arg(as_of)::timestamptz
         THEN lot.remaining ELSE LEAST(ceil(g.amount::numeric * LEAST(r.pending,(g.spec_snapshot->'deposit'->>'paid_amount')::numeric) / (g.spec_snapshot->'deposit'->>'paid_amount')::numeric),lot.remaining) END)
       FROM (SELECT refunded_payment_id, sum(-refund.amount)::numeric AS pending
         FROM billing.payments refund WHERE refund.merchant_id=sqlc.arg(merchant_id)::uuid
           AND refund.customer_id=sqlc.arg(customer_id)::uuid AND refund.currency=sqlc.arg(currency)::text
           AND refund.status='pending' AND refund.amount<0 AND refund.deleted_at IS NULL
         GROUP BY refunded_payment_id) r
       JOIN billing.payments p ON p.merchant_id=sqlc.arg(merchant_id)::uuid AND p.id=r.refunded_payment_id AND p.amount>0 AND p.deleted_at IS NULL
       JOIN billing.grants g ON g.merchant_id=sqlc.arg(merchant_id)::uuid AND g.payment_id=p.id
         AND g.kind='credit' AND g.event='grant'
         AND g.spec_snapshot->'deposit'->'paid_amount' IS NOT NULL
       JOIN LATERAL (SELECT GREATEST(g.amount-COALESCE(sum(CASE WHEN lt.transfer_type='credit_refund_restore' THEN -lt.amount ELSE lt.amount END),0),0)::numeric AS remaining
         FROM billing.ledger_transfers lt WHERE lt.merchant_id=g.merchant_id AND lt.grant_id=g.id
           AND lt.transfer_type IN ('credit_spend','owed_repayment','credit_expire','credit_revoke','credit_refund','credit_refund_restore')) lot ON true),0)
     -- What the customer's usage accrued and the ledger has not taken.
     + COALESCE((SELECT SUM(ua.amount - ua.posted_amount)
              FROM billing.usage_accruals ua
             WHERE ua.merchant_id = sqlc.arg(merchant_id)::uuid AND ua.customer_id = sqlc.arg(customer_id)::uuid
               AND ua.currency = sqlc.arg(currency)::text AND ua.amount > ua.posted_amount), 0)) AS claims,
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
