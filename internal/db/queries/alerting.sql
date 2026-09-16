-- ============================================================================
-- merchant_webhooks
-- ============================================================================

-- name: CreateMerchantWebhook :one
INSERT INTO openrails.merchant_webhooks (id, merchant_id, name, destination_host, secret_version, format, enabled)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(name), sqlc.arg(destination_host), sqlc.arg(secret_version)::integer, sqlc.arg(format), sqlc.arg(enabled))
RETURNING *;

-- name: RotateMerchantWebhookURL :one
UPDATE openrails.merchant_webhooks
   SET destination_host = sqlc.arg(destination_host),
       secret_version = sqlc.arg(secret_version)::integer,
       updated_at = current_timestamp
 WHERE id = sqlc.arg(id)::uuid
   AND secret_version <= sqlc.arg(secret_version)::integer
RETURNING *;

-- name: GetMerchantWebhook :one
SELECT * FROM openrails.merchant_webhooks WHERE id = $1;

-- name: ListMerchantWebhooks :many
SELECT * FROM openrails.merchant_webhooks ORDER BY created_at DESC, id;

-- name: DeleteMerchantWebhook :execrows
DELETE FROM openrails.merchant_webhooks WHERE id = $1;

-- ============================================================================
-- notifications  (in_app bell)
-- ============================================================================

-- name: CreateMerchantNotification :one
INSERT INTO openrails.notifications (merchant_id, recipient_kind, event_type, severity, title, body, link, data)
VALUES (sqlc.arg(merchant_id)::uuid, 'merchant', 'operator.alert', sqlc.arg(severity)::text, sqlc.arg(title)::text, sqlc.arg(body)::text, sqlc.arg(link)::text, COALESCE(sqlc.narg(data)::jsonb, '{}'::jsonb))
RETURNING *;

-- name: ListMerchantNotifications :many
SELECT * FROM openrails.notifications
WHERE recipient_kind = 'merchant' AND merchant_id = openrails.current_merchant_id()
  AND (NOT sqlc.arg(unread_only)::boolean OR read_at IS NULL)
ORDER BY created_at DESC, id
LIMIT sqlc.arg(row_limit)::int;

-- name: MarkMerchantNotificationRead :execrows
UPDATE openrails.notifications
SET read_at = COALESCE(read_at, now())
WHERE recipient_kind = 'merchant' AND merchant_id = openrails.current_merchant_id() AND id = $1;

-- name: CountUnreadMerchantNotifications :one
SELECT count(*) FROM openrails.notifications WHERE recipient_kind = 'merchant' AND merchant_id = openrails.current_merchant_id() AND read_at IS NULL;
