-- name: ClaimPendingCustomerMessages :many
-- CROSS-MERCHANT: the oldest pending messages, marked queued. SKIP LOCKED
-- hands each to one dispatcher, whose transaction queues its send job.
UPDATE billing.customer_messages m
SET status = 'queued', updated_at = sqlc.arg(now)::timestamptz
FROM (
    SELECT p.merchant_id, p.id FROM billing.customer_messages p
    WHERE p.status = 'pending'
    ORDER BY p.created_at, p.merchant_id, p.id
    LIMIT sqlc.arg(batch)::int
    FOR UPDATE SKIP LOCKED
) due
WHERE m.merchant_id = due.merchant_id AND m.id = due.id
RETURNING m.merchant_id, m.id;

-- name: GetCustomerMessage :one
-- A message and the fact it is about; the fact is absent once pruned.
SELECT m.merchant_id, m.id, m.customer_id, m.event_id, m.event_type, m.status, m.attempts, e.data, e.created_at AS occurred_at
FROM billing.customer_messages m
LEFT JOIN billing.notification_events e ON e.merchant_id = m.merchant_id AND e.id = m.event_id
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.id = sqlc.arg(id)::uuid;

-- name: ListCustomerMessages :many
-- A customer's messages, newest first.
SELECT * FROM billing.customer_messages
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND (sqlc.narg(after_at)::timestamptz IS NULL
   OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: CustomerMessageOnTestPSP :one
-- Whether the fact's PSP, payment, order or subscription is on a PSP that
-- moves no real money.
SELECT EXISTS (
    SELECT 1 FROM billing.psps p
    WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND NOT p.live
      AND p.id IN (
        SELECT pm.psp_id FROM billing.payments pm WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid AND pm.id = sqlc.narg(payment_id)::uuid
        UNION ALL
        SELECT o.psp_id FROM billing.orders o WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.id = sqlc.narg(order_id)::uuid
        UNION ALL
        SELECT s.psp_id FROM billing.subscriptions s WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.id = sqlc.narg(subscription_id)::uuid
        UNION ALL
        SELECT sqlc.narg(psp_id)::uuid
      )
)::boolean AS test_environment;

-- name: LockCustomerMessages :exec
-- Serializes one customer's sends, so the hourly cap counts each once.
SELECT pg_advisory_xact_lock(hashtextextended('openrails.customer_messages:' || sqlc.arg(merchant_id)::uuid::text || ':' || sqlc.arg(customer_id)::uuid::text, 0));

-- name: CountCustomerMessagesSince :one
-- The customer's other messages sent, or being sent, since the instant.
SELECT count(*) FROM billing.customer_messages
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND id <> sqlc.arg(id)::uuid
  AND ((status = 'sent' AND sent_at > sqlc.arg(since)::timestamptz) OR (status = 'sending' AND updated_at > sqlc.arg(since)::timestamptz));

-- name: SettleCustomerMessage :execrows
-- Moves a message its job holds: to sending, back to queued, or to where it
-- ends. A message already ended is left alone.
UPDATE billing.customer_messages
SET status = sqlc.arg(status)::text, attempts = sqlc.arg(attempts)::int,
    recipient = COALESCE(sqlc.narg(recipient)::text, recipient), last_error_code = sqlc.narg(error_code)::text,
    sent_at = sqlc.narg(sent_at)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status IN ('queued', 'sending');

-- name: EraseCustomerMessages :exec
DELETE FROM billing.customer_messages WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid;

-- name: CountMerchantCustomerMessages :one
SELECT count(*) FROM billing.customer_messages WHERE merchant_id = $1;

-- name: PurgeMerchantCustomerMessages :exec
DELETE FROM billing.customer_messages WHERE merchant_id = $1;
