-- Private producer-journal retention. The public event log has independent
-- bounded retention; neither source keeps consumer state.
-- name: PruneNotificationJournal :execrows
DELETE FROM billing.notification_events WHERE (merchant_id,id) IN (
 SELECT merchant_id,id FROM billing.notification_events WHERE inserted_at < sqlc.arg(cutoff)::timestamptz
 ORDER BY inserted_at,merchant_id,id LIMIT sqlc.arg(row_limit)::int
);

-- name: EraseCustomerNotificationEvents :exec
DELETE FROM billing.notification_events WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND customer_id=sqlc.arg(customer_id)::uuid;

-- name: CountMerchantNotificationEvents :one
SELECT count(*) FROM billing.notification_events WHERE merchant_id=$1;
-- name: PurgeMerchantNotificationEvents :exec
DELETE FROM billing.notification_events WHERE merchant_id=$1;
