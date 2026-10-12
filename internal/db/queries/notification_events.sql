-- Consumer registration and GC serialize without blocking domain producers.
-- name: LockNotificationConsumers :exec
SELECT pg_advisory_xact_lock(hashtextextended('openrails.notification-consumers',0));

-- name: CountNotificationConsumers :one
SELECT count(*) FROM billing.notification_consumers WHERE merchant_id IS NOT DISTINCT FROM sqlc.narg(merchant_id)::uuid AND owner=sqlc.arg(owner)::text;
-- name: GetNotificationConsumer :one
SELECT * FROM billing.notification_consumers WHERE merchant_id IS NOT DISTINCT FROM sqlc.narg(merchant_id)::uuid AND owner=sqlc.arg(owner)::text AND name=sqlc.arg(name)::text;
-- name: RegisterNotificationConsumer :exec
INSERT INTO billing.notification_consumers(merchant_id,owner,name) VALUES(sqlc.narg(merchant_id)::uuid,sqlc.arg(owner)::text,sqlc.arg(name)::text) ON CONFLICT DO NOTHING;
-- name: RetireNotificationConsumer :exec
DELETE FROM billing.notification_consumers WHERE merchant_id IS NOT DISTINCT FROM sqlc.narg(merchant_id)::uuid AND owner=sqlc.arg(owner)::text AND name=sqlc.arg(name)::text;
-- name: PendingNotificationEvents :many
SELECT e.* FROM billing.notification_events e JOIN billing.notification_consumers c ON c.id=sqlc.arg(consumer_id)::uuid
WHERE (c.merchant_id IS NULL OR c.merchant_id=e.merchant_id)
AND NOT EXISTS(SELECT 1 FROM billing.notification_event_acks a WHERE a.consumer_id=c.id AND a.merchant_id=e.merchant_id AND a.event_id=e.id)
ORDER BY e.inserted_at,e.merchant_id,e.id LIMIT sqlc.arg(row_limit)::int;
-- name: AcknowledgeNotificationEvent :exec
INSERT INTO billing.notification_event_acks(consumer_id,merchant_id,event_id)
SELECT c.id,e.merchant_id,e.id FROM billing.notification_consumers c JOIN billing.notification_events e ON (c.merchant_id IS NULL OR c.merchant_id=e.merchant_id)
WHERE c.id=sqlc.arg(consumer_id)::uuid AND e.merchant_id=sqlc.arg(merchant_id)::uuid AND e.id=sqlc.arg(event_id)::uuid
ON CONFLICT DO NOTHING;
-- name: PruneNotificationEvents :execrows
DELETE FROM billing.notification_events e WHERE (merchant_id,id) IN (
 SELECT x.merchant_id,x.id FROM billing.notification_events x WHERE x.inserted_at<sqlc.arg(cutoff)::timestamptz
 AND NOT EXISTS(SELECT 1 FROM billing.notification_consumers c WHERE (c.merchant_id IS NULL OR c.merchant_id=x.merchant_id)
 AND NOT EXISTS(SELECT 1 FROM billing.notification_event_acks a WHERE a.consumer_id=c.id AND a.merchant_id=x.merchant_id AND a.event_id=x.id AND a.acknowledged_at<sqlc.arg(cutoff)::timestamptz))
 ORDER BY x.inserted_at,x.merchant_id,x.id LIMIT sqlc.arg(row_limit)::int);

-- name: EraseCustomerNotificationEvents :exec
DELETE FROM billing.notification_events WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND customer_id=sqlc.arg(customer_id)::uuid;

-- name: CountMerchantNotificationEvents :one
SELECT count(*) FROM billing.notification_events WHERE merchant_id=$1;
-- name: PurgeMerchantNotificationEvents :exec
DELETE FROM billing.notification_events WHERE merchant_id=$1;

-- name: CountMerchantNotificationEventAcks :one
SELECT count(*) FROM billing.notification_event_acks WHERE merchant_id=$1;
-- name: PurgeMerchantNotificationEventAcks :exec
DELETE FROM billing.notification_event_acks WHERE merchant_id=$1;

-- name: CountMerchantNotificationConsumers :one
SELECT count(*) FROM billing.notification_consumers WHERE merchant_id=$1;
-- name: PurgeMerchantNotificationConsumers :exec
DELETE FROM billing.notification_consumers WHERE merchant_id=$1;
