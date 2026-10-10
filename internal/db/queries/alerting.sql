-- ============================================================================
-- notifications  (in_app bell)
-- ============================================================================

-- A given id makes the write idempotent: a second write of it is a no-op.
-- name: CreateMerchantNotification :execrows
INSERT INTO billing.notifications (merchant_id, id, recipient_kind, event_type, severity, title, body, link, data)
VALUES (sqlc.arg(merchant_id)::uuid, COALESCE(sqlc.narg(id)::uuid, uuidv7()), 'merchant', 'operator.alert', sqlc.arg(severity)::text, sqlc.arg(title)::text, sqlc.arg(body)::text, NULLIF(sqlc.arg(link)::text, ''), COALESCE(sqlc.narg(data)::jsonb, '{}'::jsonb))
ON CONFLICT (merchant_id, id) DO NOTHING;

-- name: ListMerchantNotifications :many
SELECT * FROM billing.notifications
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND recipient_kind = 'merchant'
  AND (NOT sqlc.arg(unread_only)::boolean OR read_at IS NULL)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListMerchantNotificationsByIDs :many
SELECT * FROM billing.notifications
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[]) AND recipient_kind = 'merchant'
ORDER BY created_at DESC, id DESC;

-- name: MarkMerchantNotificationsRead :many
UPDATE billing.notifications
SET read_at = COALESCE(read_at, now())
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND recipient_kind = 'merchant' AND id = ANY(sqlc.arg(ids)::uuid[])
RETURNING *;

-- name: CountUnreadMerchantNotifications :one
SELECT count(*) FROM billing.notifications
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND recipient_kind = 'merchant' AND read_at IS NULL;
