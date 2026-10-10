-- billing.notifications.

-- name: CreateNotification :execrows
INSERT INTO billing.notifications (
    id, merchant_id, customer_id, event_type, data, read_at, created_at
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(event_type)::text, COALESCE(sqlc.narg(data), '{}'::jsonb), CASE WHEN sqlc.arg(seen)::boolean THEN now() END,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: CreateNotificationIfAbsent :exec
INSERT INTO billing.notifications (
    id, merchant_id, customer_id, event_type, data, read_at, created_at
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(event_type)::text, COALESCE(sqlc.narg(data), '{}'::jsonb), CASE WHEN sqlc.arg(seen)::boolean THEN now() END,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
)
ON CONFLICT (merchant_id, id) DO NOTHING;

-- name: GetNotificationByID :one
SELECT * FROM billing.notifications WHERE recipient_kind = 'customer' AND merchant_id = billing.current_merchant_id() AND id = $1;

-- name: ListNotificationsByCustomer :many
SELECT * FROM billing.notifications nq
WHERE nq.recipient_kind = 'customer' AND nq.merchant_id = billing.current_merchant_id() AND nq.customer_id = sqlc.arg(customer_id)::uuid
ORDER BY nq.created_at DESC;

-- name: UpdateNotification :execrows
UPDATE billing.notifications SET
    customer_id = sqlc.arg(customer_id)::uuid,
    event_type = sqlc.arg(event_type)::text,
    data = sqlc.narg(data),
    read_at = CASE WHEN sqlc.arg(seen)::boolean THEN COALESCE(read_at, now()) END
WHERE recipient_kind = 'customer' AND merchant_id = billing.current_merchant_id() AND id = $1;

-- name: DeleteNotification :execrows
DELETE FROM billing.notifications WHERE recipient_kind = 'customer' AND merchant_id = billing.current_merchant_id() AND id = $1;

-- name: CountNotificationsFiltered :one
SELECT count(*) FROM billing.notifications nq
WHERE nq.recipient_kind = 'customer' AND nq.merchant_id = billing.current_merchant_id()
  AND (sqlc.narg(customer_id)::uuid IS NULL OR nq.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(event_type)::text IS NULL OR nq.event_type = sqlc.narg(event_type)::text)
  AND (sqlc.narg(seen)::boolean IS NULL OR (nq.read_at IS NOT NULL) = sqlc.narg(seen)::boolean);

-- name: ListNotificationsFiltered :many
SELECT * FROM billing.notifications nq
WHERE nq.recipient_kind = 'customer' AND nq.merchant_id = billing.current_merchant_id()
  AND (sqlc.narg(customer_id)::uuid IS NULL OR nq.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(event_type)::text IS NULL OR nq.event_type = sqlc.narg(event_type)::text)
  AND (sqlc.narg(seen)::boolean IS NULL OR (nq.read_at IS NOT NULL) = sqlc.narg(seen)::boolean)
ORDER BY nq.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- Retention sweeps, one merchant per pass: the explicit merchant predicate keeps
-- a DELETE from crossing merchants. row_limit bounds one statement and
-- transaction; the caller loops until a short batch.
-- name: DeleteSeenNotificationsBefore :execrows
DELETE FROM billing.notifications
WHERE ctid IN (
    SELECT nq.ctid FROM billing.notifications nq
    WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
      AND nq.read_at IS NOT NULL AND nq.created_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);

-- name: DeleteNotificationsBefore :execrows
DELETE FROM billing.notifications
WHERE ctid IN (
    SELECT nq.ctid FROM billing.notifications nq
    WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
      AND nq.created_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);

-- Dedupe guard for the converge NOTIFY pass: a premium_ended row created at or
-- after the window close means the customer was already told.
-- name: PremiumEndedNotificationExistsSince :one
SELECT EXISTS (
    SELECT 1 FROM billing.notifications nq
    WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
      AND nq.customer_id = sqlc.arg(customer_id)::uuid
      AND nq.event_type = 'premium_ended'
      AND nq.created_at >= sqlc.arg(since)::timestamptz
)::boolean AS found;

-- Undelivered rows for the notification email sweep (emailed_at NULL).
-- name: ListUndeliveredNotifications :many
SELECT * FROM billing.notifications nq
WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
  AND nq.recipient_kind = 'customer' AND nq.emailed_at IS NULL
  AND (sqlc.narg(after_created_at)::timestamptz IS NULL
       OR (nq.created_at, nq.id) > (sqlc.narg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid))
ORDER BY nq.created_at, nq.id
LIMIT sqlc.arg(page_limit)::int;

-- name: MarkNotificationEmailed :execrows
UPDATE billing.notifications
SET emailed_at = sqlc.arg(emailed_at)::timestamptz
WHERE recipient_kind = 'customer' AND merchant_id = billing.current_merchant_id() AND id = $1 AND emailed_at IS NULL;

-- One sender at a time: claims an undelivered email for lease. No row means
-- it was sent or another sender holds it.
-- name: LeaseNotificationEmail :execrows
UPDATE billing.notifications
SET email_lease_expires_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::float8)
WHERE recipient_kind = 'customer' AND merchant_id = billing.current_merchant_id() AND id = sqlc.arg(id)::uuid
  AND emailed_at IS NULL AND (email_lease_expires_at IS NULL OR email_lease_expires_at <= now());

-- A sender that failed hands the email back to the next one.
-- name: ReleaseNotificationEmail :exec
UPDATE billing.notifications
SET email_lease_expires_at = NULL
WHERE recipient_kind = 'customer' AND merchant_id = billing.current_merchant_id() AND id = $1 AND emailed_at IS NULL;

-- A customer's own notifications, newest first.
-- name: ListCustomerNotifications :many
SELECT * FROM billing.notifications nq
WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid AND nq.recipient_kind = 'customer' AND nq.customer_id = sqlc.arg(customer_id)::uuid
  AND (sqlc.narg(seen)::boolean IS NULL OR (nq.read_at IS NOT NULL) = sqlc.narg(seen)::boolean)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (nq.created_at, nq.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY nq.created_at DESC, nq.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: CountUnreadCustomerNotifications :one
SELECT count(*) FROM billing.notifications
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND recipient_kind = 'customer' AND customer_id = sqlc.arg(customer_id)::uuid AND read_at IS NULL;

-- name: MarkCustomerNotificationsRead :many
-- Only the customer's own notifications: another customer's id matches none.
UPDATE billing.notifications SET read_at = COALESCE(read_at, now())
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
  AND recipient_kind = 'customer' AND customer_id = sqlc.arg(customer_id)::uuid
RETURNING *;

-- name: MarkAllCustomerNotificationsRead :execrows
UPDATE billing.notifications SET read_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND recipient_kind = 'customer'
  AND customer_id = sqlc.arg(customer_id)::uuid AND read_at IS NULL;

-- Renewal-receipt throttle: a receipt for this subscription whose renewal
-- period started after since.
-- name: RenewalReceiptSince :one
SELECT EXISTS (
    SELECT 1 FROM billing.notifications nq
    WHERE nq.recipient_kind = 'customer'
      AND nq.merchant_id = billing.current_merchant_id()
      AND nq.customer_id = sqlc.arg(customer_id)::uuid
      AND nq.event_type = 'premium_renewed'
      AND nq.data->>'subscription_id' = sqlc.arg(subscription_id)::text
      AND (nq.data->>'period_starts_at')::timestamptz > sqlc.arg(since)::timestamptz
)::boolean AS found;
