-- Immutable customer notification facts.
-- name: CreateNotification :execrows
INSERT INTO billing.notification_events (id,merchant_id,customer_id,event_type,data,created_at)
VALUES(sqlc.arg(id)::uuid,sqlc.arg(merchant_id)::uuid,sqlc.arg(customer_id)::uuid,sqlc.arg(event_type)::text,COALESCE(sqlc.narg(data),'{}'::jsonb),
COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz,'0001-01-01 00:00:00+00'::timestamptz),now()));

-- name: CreateNotificationIfAbsent :exec
INSERT INTO billing.notification_events (id,merchant_id,customer_id,event_type,data,created_at)
VALUES(sqlc.arg(id)::uuid,sqlc.arg(merchant_id)::uuid,sqlc.arg(customer_id)::uuid,sqlc.arg(event_type)::text,COALESCE(sqlc.narg(data),'{}'::jsonb),
COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz,'0001-01-01 00:00:00+00'::timestamptz),now()))
ON CONFLICT(merchant_id,id) DO NOTHING;

-- name: PremiumEndedNotificationExistsSince :one
SELECT EXISTS(SELECT 1 FROM billing.notification_events WHERE merchant_id=sqlc.arg(merchant_id)::uuid
AND customer_id=sqlc.arg(customer_id)::uuid AND event_type='premium_ended' AND created_at>=sqlc.arg(since)::timestamptz)::boolean AS found;

-- Legacy merchant alerts retain their existing retention, outside customer facts.
-- name: DeleteSeenNotificationsBefore :execrows
DELETE FROM billing.notifications WHERE ctid IN (SELECT ctid FROM billing.notifications WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND read_at IS NOT NULL AND created_at<sqlc.arg(cutoff)::timestamptz LIMIT sqlc.arg(row_limit)::int);
-- name: DeleteNotificationsBefore :execrows
DELETE FROM billing.notifications WHERE ctid IN (SELECT ctid FROM billing.notifications WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND created_at<sqlc.arg(cutoff)::timestamptz LIMIT sqlc.arg(row_limit)::int);
