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

-- name: ListUnseenNotificationsByCustomer :many
SELECT * FROM openrails.notification_queue nq
WHERE nq.customer_id = $1 AND nq.seen = false
ORDER BY nq.created_at DESC;

-- name: ListNotificationsByEventType :many
SELECT * FROM openrails.notification_queue nq
WHERE nq.event_type = $1
ORDER BY nq.created_at DESC;

-- name: CountNotificationsByCustomerEventSince :one
SELECT count(*) FROM openrails.notification_queue nq
WHERE nq.customer_id = $1
  AND nq.event_type = $2
  AND nq.created_at >= $3;

-- name: ListCustomersWithPendingDigest :many
SELECT DISTINCT nq.customer_id::text FROM openrails.notification_queue nq
WHERE nq.event_type = $1
  AND nq.created_at >= $2;

-- name: ListPendingDigestForCustomer :many
SELECT * FROM openrails.notification_queue nq
WHERE nq.customer_id = $1
  AND nq.event_type = $2
  AND nq.created_at >= $3
ORDER BY nq.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0);

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

-- name: DeleteSeenNotificationsBefore :execrows
DELETE FROM openrails.notification_queue
WHERE seen = true AND created_at < sqlc.arg(cutoff)::timestamptz;

-- name: DeleteNotificationsBefore :execrows
DELETE FROM openrails.notification_queue
WHERE created_at < sqlc.arg(cutoff)::timestamptz;

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
ORDER BY nq.created_at
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
