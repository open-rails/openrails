-- openrails.notifications.

-- name: CreateNotification :execrows
INSERT INTO openrails.notifications (
    id, merchant_id, customer_id, event_type, data, read_at, created_at
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(event_type)::text, COALESCE(sqlc.narg(data), '{}'::jsonb), CASE WHEN sqlc.arg(seen)::boolean THEN now() END,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: CreateNotificationIfAbsent :exec
INSERT INTO openrails.notifications (
    id, merchant_id, customer_id, event_type, data, read_at, created_at
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(event_type)::text, COALESCE(sqlc.narg(data), '{}'::jsonb), CASE WHEN sqlc.arg(seen)::boolean THEN now() END,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
)
ON CONFLICT (id) DO NOTHING;

-- name: GetNotificationByID :one
SELECT * FROM openrails.notifications WHERE recipient_kind = 'customer' AND merchant_id = openrails.current_merchant_id() AND id = $1;

-- name: ListNotificationsByCustomer :many
SELECT * FROM openrails.notifications nq
WHERE nq.recipient_kind = 'customer' AND nq.merchant_id = openrails.current_merchant_id() AND nq.customer_id = sqlc.arg(customer_id)::uuid
ORDER BY nq.created_at DESC;

-- name: MarkNotificationSeen :execrows
UPDATE openrails.notifications SET read_at = COALESCE(read_at, now())
WHERE recipient_kind = 'customer' AND merchant_id = openrails.current_merchant_id()
  AND id = sqlc.arg(id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid;

-- name: UpdateNotification :execrows
UPDATE openrails.notifications SET
    customer_id = sqlc.arg(customer_id)::uuid,
    event_type = sqlc.arg(event_type)::text,
    data = sqlc.narg(data),
    read_at = CASE WHEN sqlc.arg(seen)::boolean THEN COALESCE(read_at, now()) END
WHERE recipient_kind = 'customer' AND merchant_id = openrails.current_merchant_id() AND id = $1;

-- name: DeleteNotification :execrows
DELETE FROM openrails.notifications WHERE recipient_kind = 'customer' AND merchant_id = openrails.current_merchant_id() AND id = $1;

-- name: CountNotificationsFiltered :one
SELECT count(*) FROM openrails.notifications nq
WHERE nq.recipient_kind = 'customer' AND nq.merchant_id = openrails.current_merchant_id()
  AND (sqlc.narg(customer_id)::uuid IS NULL OR nq.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(event_type)::text IS NULL OR nq.event_type = sqlc.narg(event_type)::text)
  AND (sqlc.narg(seen)::boolean IS NULL OR (nq.read_at IS NOT NULL) = sqlc.narg(seen)::boolean);

-- name: ListNotificationsFiltered :many
SELECT * FROM openrails.notifications nq
WHERE nq.recipient_kind = 'customer' AND nq.merchant_id = openrails.current_merchant_id()
  AND (sqlc.narg(customer_id)::uuid IS NULL OR nq.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(event_type)::text IS NULL OR nq.event_type = sqlc.narg(event_type)::text)
  AND (sqlc.narg(seen)::boolean IS NULL OR (nq.read_at IS NOT NULL) = sqlc.narg(seen)::boolean)
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
DELETE FROM openrails.notifications
WHERE ctid IN (
    SELECT nq.ctid FROM openrails.notifications nq
    WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
      AND nq.read_at IS NOT NULL AND nq.created_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);

-- name: DeleteNotificationsBefore :execrows
DELETE FROM openrails.notifications
WHERE ctid IN (
    SELECT nq.ctid FROM openrails.notifications nq
    WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
      AND nq.created_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);

-- #789: dedupe guard for the converge NOTIFY pass — any premium_ended row
-- created at/after the window close means the customer was already told.
-- name: PremiumEndedNotificationExistsSince :one
SELECT EXISTS (
    SELECT 1 FROM openrails.notifications nq
    WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
      AND nq.customer_id = sqlc.arg(customer_id)::uuid
      AND nq.event_type = 'premium_ended'
      AND nq.created_at >= sqlc.arg(since)::timestamptz
)::boolean AS found;

-- #789: undelivered rows for the notification email sweep (emailed_at NULL).
-- name: ListUndeliveredNotifications :many
SELECT * FROM openrails.notifications nq
WHERE nq.merchant_id = sqlc.arg(merchant_id)::uuid
  AND nq.recipient_kind = 'customer' AND nq.emailed_at IS NULL
  AND (sqlc.narg(after_created_at)::timestamptz IS NULL
       OR (nq.created_at, nq.id) > (sqlc.narg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid))
ORDER BY nq.created_at, nq.id
LIMIT sqlc.arg(page_limit)::int;

-- name: MarkNotificationEmailed :execrows
UPDATE openrails.notifications
SET emailed_at = sqlc.arg(emailed_at)::timestamptz
WHERE recipient_kind = 'customer' AND merchant_id = openrails.current_merchant_id() AND id = $1 AND emailed_at IS NULL;

-- name: CountRepairAlerts :one
SELECT count(*) FROM openrails.notifications nq
WHERE nq.recipient_kind = 'customer' AND nq.merchant_id = openrails.current_merchant_id() AND nq.customer_id = sqlc.arg(customer_id)::uuid
  AND nq.event_type = $2
  AND nq.data ->> 'kind' = 'billing_ledger_repair_required'
  AND (sqlc.narg(seen)::boolean IS NULL OR (nq.read_at IS NOT NULL) = sqlc.narg(seen)::boolean);

-- name: ListRepairAlerts :many
SELECT * FROM openrails.notifications nq
WHERE nq.recipient_kind = 'customer' AND nq.merchant_id = openrails.current_merchant_id() AND nq.customer_id = sqlc.arg(customer_id)::uuid
  AND nq.event_type = $2
  AND nq.data ->> 'kind' = 'billing_ledger_repair_required'
  AND (sqlc.narg(seen)::boolean IS NULL OR (nq.read_at IS NOT NULL) = sqlc.narg(seen)::boolean)
ORDER BY nq.created_at DESC
LIMIT $3::int OFFSET $4::int;
