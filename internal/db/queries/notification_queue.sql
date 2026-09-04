-- openrails.notification_queue.

-- name: CreateNotification :execrows
INSERT INTO openrails.notification_queue (
    id, merchant_id, customer_id, event_type, data, seen, created_at
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, COALESCE(sqlc.narg(data), '{}'::jsonb), $4,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: CreateNotificationIfAbsent :exec
INSERT INTO openrails.notification_queue (
    id, merchant_id, customer_id, event_type, data, seen, created_at
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, COALESCE(sqlc.narg(data), '{}'::jsonb), $4,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
)
ON CONFLICT (id) DO NOTHING;

-- name: GetNotificationByID :one
SELECT * FROM openrails.notification_queue WHERE id = $1;

-- name: ListNotificationsByCustomer :many
SELECT * FROM openrails.notification_queue nq
WHERE nq.customer_id = $1
ORDER BY nq.created_at DESC;

-- name: MarkNotificationSeen :execrows
UPDATE openrails.notification_queue SET seen = true WHERE id = $1;

-- name: UpdateNotification :execrows
UPDATE openrails.notification_queue SET
    customer_id = $2,
    event_type = $3,
    data = sqlc.narg(data),
    seen = $4
WHERE id = $1;

-- name: DeleteNotification :execrows
DELETE FROM openrails.notification_queue WHERE id = $1;

-- name: CountNotificationsFiltered :one
SELECT count(*) FROM openrails.notification_queue nq
WHERE (sqlc.narg(customer_id)::uuid IS NULL OR nq.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(event_type)::text IS NULL OR nq.event_type = sqlc.narg(event_type)::text)
  AND (sqlc.narg(seen)::boolean IS NULL OR nq.seen = sqlc.narg(seen)::boolean);

-- name: ListNotificationsFiltered :many
SELECT * FROM openrails.notification_queue nq
WHERE (sqlc.narg(customer_id)::uuid IS NULL OR nq.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(event_type)::text IS NULL OR nq.event_type = sqlc.narg(event_type)::text)
  AND (sqlc.narg(seen)::boolean IS NULL OR nq.seen = sqlc.narg(seen)::boolean)
ORDER BY nq.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- Retention sweeps (or#877 B4). The merchant predicate is explicit, not
-- implied: the sweep walks the merchant directory and runs one pass per
-- merchant, and an unqualified DELETE would be a cross-merchant delete the
-- moment it ran on a BYPASSRLS connection (a superuser self-host, a test).
--
-- or#837: BATCHED. row_limit bounds one statement (and so one transaction);
-- the caller loops until a short batch comes back. A merchant with a year of
-- unswept notifications used to be one DELETE holding a transaction — and the
-- table's dead tuples — open for as long as it took.
-- name: DeleteSeenNotificationsBefore :execrows
DELETE FROM openrails.notification_queue
WHERE ctid IN (
    SELECT nq.ctid FROM openrails.notification_queue nq
    WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
      AND nq.seen = true AND nq.created_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);

-- name: DeleteNotificationsBefore :execrows
DELETE FROM openrails.notification_queue
WHERE ctid IN (
    SELECT nq.ctid FROM openrails.notification_queue nq
    WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
      AND nq.created_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);

-- #789: dedupe guard for the converge NOTIFY pass — any premium_ended row
-- created at/after the window close means the customer was already told.
-- name: PremiumEndedNotificationExistsSince :one
SELECT EXISTS (
    SELECT 1 FROM openrails.notification_queue nq
    WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
      AND nq.customer_id = sqlc.arg(customer_id)::uuid
      AND nq.event_type = 'premium_ended'
      AND nq.created_at >= sqlc.arg(since)::timestamptz
)::boolean AS found;

-- #789: undelivered rows for the notification email sweep (emailed_at NULL).
-- name: ListUndeliveredNotifications :many
SELECT * FROM openrails.notification_queue nq
WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
  AND nq.emailed_at IS NULL
  AND (sqlc.narg(after_created_at)::timestamptz IS NULL
       OR (nq.created_at, nq.id) > (sqlc.narg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid))
ORDER BY nq.created_at, nq.id
LIMIT sqlc.arg(page_limit)::int;

-- name: MarkNotificationEmailed :execrows
UPDATE openrails.notification_queue
SET emailed_at = sqlc.arg(emailed_at)::timestamptz
WHERE id = $1 AND emailed_at IS NULL;

-- name: CountRepairAlerts :one
SELECT count(*) FROM openrails.notification_queue nq
WHERE nq.customer_id = $1
  AND nq.event_type = $2
  AND nq.data ->> 'kind' = 'billing_ledger_repair_required'
  AND (sqlc.narg(seen)::boolean IS NULL OR nq.seen = sqlc.narg(seen)::boolean);

-- name: ListRepairAlerts :many
SELECT * FROM openrails.notification_queue nq
WHERE nq.customer_id = $1
  AND nq.event_type = $2
  AND nq.data ->> 'kind' = 'billing_ledger_repair_required'
  AND (sqlc.narg(seen)::boolean IS NULL OR nq.seen = sqlc.narg(seen)::boolean)
ORDER BY nq.created_at DESC
LIMIT $3::int OFFSET $4::int;
