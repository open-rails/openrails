-- ============================================================================
-- merchant_webhooks
-- ============================================================================

-- name: CreateMerchantWebhook :one
INSERT INTO billing.merchant_webhooks (id, merchant_id, name, destination_host, secret_version, format, enabled)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(name), sqlc.arg(destination_host), sqlc.arg(secret_version)::integer, sqlc.arg(format), sqlc.arg(enabled))
RETURNING *;

-- name: RotateMerchantWebhookURL :one
UPDATE billing.merchant_webhooks
   SET destination_host = sqlc.arg(destination_host),
       secret_version = sqlc.arg(secret_version)::integer,
       updated_at = current_timestamp
 WHERE merchant_webhooks.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
   AND secret_version <= sqlc.arg(secret_version)::integer
RETURNING *;

-- name: GetMerchantWebhook :one
SELECT * FROM billing.merchant_webhooks WHERE merchant_webhooks.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- name: ListMerchantWebhooks :many
SELECT * FROM billing.merchant_webhooks
WHERE merchant_webhooks.merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY created_at DESC, id;

-- name: DeleteMerchantWebhook :execrows
DELETE FROM billing.merchant_webhooks WHERE merchant_webhooks.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- ============================================================================
-- notifications  (in_app bell)
-- ============================================================================

-- name: CreateMerchantNotification :one
INSERT INTO billing.notifications (merchant_id, recipient_kind, event_type, severity, title, body, link, data)
VALUES (sqlc.arg(merchant_id)::uuid, 'merchant', 'operator.alert', sqlc.arg(severity)::text, sqlc.arg(title)::text, sqlc.arg(body)::text, sqlc.arg(link)::text, COALESCE(sqlc.narg(data)::jsonb, '{}'::jsonb))
RETURNING *;

-- name: ListMerchantNotifications :many
SELECT * FROM billing.notifications
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND recipient_kind = 'merchant'
  AND (NOT sqlc.arg(unread_only)::boolean OR read_at IS NULL)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: MarkMerchantNotificationRead :one
UPDATE billing.notifications
SET read_at = COALESCE(read_at, now())
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND recipient_kind = 'merchant' AND id = sqlc.arg(id)::uuid
RETURNING *;

-- name: CountUnreadMerchantNotifications :one
SELECT count(*) FROM billing.notifications
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND recipient_kind = 'merchant' AND read_at IS NULL;
