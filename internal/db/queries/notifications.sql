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

-- The access ledger, rather than billing status, determines whether stopping a
-- subscription actually ended membership. A future grant after a gap does not
-- mean that access remains active now.
-- name: SubscriptionNotificationAccess :one
SELECT COALESCE(bool_or(starts_at <= sqlc.arg(at_time)::timestamptz AND closes_at > sqlc.arg(at_time)::timestamptz), false)::boolean AS retained,
       CASE WHEN bool_or(starts_at <= sqlc.arg(at_time)::timestamptz AND closes_at = 'infinity'::timestamptz) THEN '0001-01-01 00:00:00+00'::timestamptz
            ELSE COALESCE(max(closes_at) FILTER (WHERE starts_at <= sqlc.arg(at_time)::timestamptz AND closes_at > sqlc.arg(at_time)::timestamptz), '0001-01-01 00:00:00+00'::timestamptz) END::timestamptz AS retained_until,
       COALESCE(max(closes_at) FILTER (WHERE closes_at <= sqlc.arg(at_time)::timestamptz), '0001-01-01 00:00:00+00'::timestamptz)::timestamptz AS closed_at
FROM (
    SELECT starts_at, LEAST(COALESCE(ends_at, 'infinity'::timestamptz), COALESCE(revoked_at, 'infinity'::timestamptz)) AS closes_at
    FROM billing.product_access access
    WHERE access.merchant_id = sqlc.arg(merchant_id)::uuid AND access.customer_id = sqlc.arg(customer_id)::uuid
      AND access.deleted_at IS NULL
      AND access.product_id IN (
          SELECT owned.product_id FROM billing.product_access owned
          WHERE owned.merchant_id = access.merchant_id AND owned.customer_id = access.customer_id
            AND owned.source_type IN ('subscription', 'grace') AND owned.source_id = sqlc.arg(subscription_id)::text
            AND owned.deleted_at IS NULL
      )
) AS windows;

-- name: PremiumEndedNotificationExistsSince :one
SELECT EXISTS(SELECT 1 FROM billing.notification_events WHERE merchant_id=sqlc.arg(merchant_id)::uuid
AND customer_id=sqlc.arg(customer_id)::uuid AND event_type='premium_ended' AND created_at>=sqlc.arg(since)::timestamptz)::boolean AS found;

-- Legacy merchant alerts retain their existing retention, outside customer facts.
-- name: DeleteSeenNotificationsBefore :execrows
DELETE FROM billing.notifications WHERE ctid IN (SELECT ctid FROM billing.notifications WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND read_at IS NOT NULL AND created_at<sqlc.arg(cutoff)::timestamptz LIMIT sqlc.arg(row_limit)::int);
-- name: DeleteNotificationsBefore :execrows
DELETE FROM billing.notifications WHERE ctid IN (SELECT ctid FROM billing.notifications WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND created_at<sqlc.arg(cutoff)::timestamptz LIMIT sqlc.arg(row_limit)::int);
