-- openrails.notification_queue.

-- name: CreateNotification :execrows
INSERT INTO openrails.notification_queue (
    id, merchant_id, merchant_subject_id, event_type, data, seen, created_at
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, COALESCE(sqlc.narg(data), '{}'::jsonb), $4,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetNotificationByID :one
SELECT * FROM openrails.notification_queue WHERE id = $1;

-- name: ListNotificationsByMerchantSubject :many
SELECT * FROM openrails.notification_queue nq
WHERE nq.merchant_subject_id = $1
ORDER BY nq.created_at DESC;

-- name: ListUnseenNotificationsByMerchantSubject :many
SELECT * FROM openrails.notification_queue nq
WHERE nq.merchant_subject_id = $1 AND nq.seen = false
ORDER BY nq.created_at DESC;

-- name: ListNotificationsByEventType :many
SELECT * FROM openrails.notification_queue nq
WHERE nq.event_type = $1
ORDER BY nq.created_at DESC;

-- name: CountNotificationsByMerchantSubjectEventSince :one
SELECT count(*) FROM openrails.notification_queue nq
WHERE nq.merchant_subject_id = $1
  AND nq.event_type = $2
  AND nq.created_at >= $3;

-- name: ListMerchantSubjectsWithPendingDigest :many
SELECT DISTINCT nq.merchant_subject_id::text FROM openrails.notification_queue nq
WHERE nq.event_type = $1
  AND nq.created_at >= $2;

-- name: ListPendingDigestForMerchantSubject :many
SELECT * FROM openrails.notification_queue nq
WHERE nq.merchant_subject_id = $1
  AND nq.event_type = $2
  AND nq.created_at >= $3
ORDER BY nq.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0);

-- name: MarkNotificationSeen :execrows
UPDATE openrails.notification_queue SET seen = true WHERE id = $1;

-- name: UpdateNotification :execrows
UPDATE openrails.notification_queue SET
    merchant_subject_id = $2,
    event_type = $3,
    data = sqlc.narg(data),
    seen = $4
WHERE id = $1;

-- name: DeleteNotification :execrows
DELETE FROM openrails.notification_queue WHERE id = $1;

-- name: CountNotificationsFiltered :one
SELECT count(*) FROM openrails.notification_queue nq
WHERE (sqlc.narg(merchant_subject_id)::uuid IS NULL OR nq.merchant_subject_id = sqlc.narg(merchant_subject_id)::uuid)
  AND (sqlc.narg(event_type)::text IS NULL OR nq.event_type = sqlc.narg(event_type)::text)
  AND (sqlc.narg(seen)::boolean IS NULL OR nq.seen = sqlc.narg(seen)::boolean);

-- name: ListNotificationsFiltered :many
SELECT * FROM openrails.notification_queue nq
WHERE (sqlc.narg(merchant_subject_id)::uuid IS NULL OR nq.merchant_subject_id = sqlc.narg(merchant_subject_id)::uuid)
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

-- name: CountRepairAlerts :one
SELECT count(*) FROM openrails.notification_queue nq
WHERE nq.merchant_subject_id = $1
  AND nq.event_type = $2
  AND nq.data ->> 'kind' = 'billing_ledger_repair_required'
  AND (sqlc.narg(seen)::boolean IS NULL OR nq.seen = sqlc.narg(seen)::boolean);

-- name: ListRepairAlerts :many
SELECT * FROM openrails.notification_queue nq
WHERE nq.merchant_subject_id = $1
  AND nq.event_type = $2
  AND nq.data ->> 'kind' = 'billing_ledger_repair_required'
  AND (sqlc.narg(seen)::boolean IS NULL OR nq.seen = sqlc.narg(seen)::boolean)
ORDER BY nq.created_at DESC
LIMIT $3::int OFFSET $4::int;
