-- name: LockUserDestructiveReservations :exec
SELECT pg_advisory_xact_lock(hashtextextended('openrails:user-destructive:' || 'billing.user_destructive_reservations'::regclass::oid::text || ':' || sqlc.arg(merchant_id)::uuid::text, 0));

-- name: UserDestructiveClock :one
SELECT clock_timestamp()::timestamptz AS at;

-- name: UserDestructiveReservation :one
SELECT customer_id,kind,reserved_at FROM billing.user_destructive_reservations
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND action_key=sqlc.arg(action_key)::text;

-- name: UserDestructiveWindow :one
WITH recent AS (
 SELECT customer_id,reserved_at FROM billing.user_destructive_reservations
 WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND reserved_at>sqlc.arg(day_since)::timestamptz
), day_customers AS (
 SELECT customer_id,max(reserved_at) AS last_at FROM recent GROUP BY customer_id
), hour_customers AS (
 SELECT customer_id,max(reserved_at) AS last_at FROM recent
 WHERE reserved_at>sqlc.arg(hour_since)::timestamptz GROUP BY customer_id
)
SELECT (SELECT count(*) FROM day_customers)::bigint AS affected_customers,
 EXISTS(SELECT 1 FROM day_customers WHERE customer_id=sqlc.arg(customer_id)::uuid)::boolean AS customer_counted,
 COALESCE((SELECT last_at FROM day_customers ORDER BY last_at,customer_id OFFSET GREATEST((SELECT count(*) FROM day_customers)-sqlc.arg(day_limit)::bigint,0) LIMIT 1), 'epoch'::timestamptz) AS oldest_customer,
 (SELECT count(*) FROM hour_customers)::bigint AS hourly_customers,
 EXISTS(SELECT 1 FROM hour_customers WHERE customer_id=sqlc.arg(customer_id)::uuid)::boolean AS customer_counted_hour,
 COALESCE((SELECT last_at FROM hour_customers ORDER BY last_at,customer_id OFFSET GREATEST((SELECT count(*) FROM hour_customers)-sqlc.arg(hour_limit)::bigint,0) LIMIT 1), 'epoch'::timestamptz) AS oldest_hour_customer;

-- name: UserDestructivePaidCustomers :one
-- Completed positive payments identify paid customers. Signups, failed attempts,
-- zero-value grants, refund events, future-dated or deleted facts never inflate it.
SELECT count(DISTINCT customer_id)::bigint FROM billing.payments
WHERE merchant_id=sqlc.arg(merchant_id)::uuid
 AND purchased_at>sqlc.arg(month_since)::timestamptz AND purchased_at<=sqlc.arg(at)::timestamptz
 AND status='succeeded' AND amount>0 AND refunded_payment_id IS NULL AND deleted_at IS NULL;

-- name: ReserveUserDestructiveAction :exec
INSERT INTO billing.user_destructive_reservations(merchant_id,customer_id,action_key,kind,reserved_at)
VALUES(sqlc.arg(merchant_id)::uuid,sqlc.arg(customer_id)::uuid,sqlc.arg(action_key)::text,sqlc.arg(kind)::text,sqlc.arg(at)::timestamptz);

-- name: PruneUserDestructiveReservations :execrows
-- Bounded global cleanup of operational state; the billing record retains action identity.
DELETE FROM billing.user_destructive_reservations r USING (
 SELECT merchant_id,action_key FROM billing.user_destructive_reservations
 WHERE reserved_at<=statement_timestamp()-interval '24 hours'
 ORDER BY reserved_at,merchant_id,action_key LIMIT sqlc.arg(batch_size)::int
 FOR UPDATE SKIP LOCKED
) old WHERE r.merchant_id=old.merchant_id AND r.action_key=old.action_key;

-- name: DeleteExpiredUserDestructiveReservation :exec
DELETE FROM billing.user_destructive_reservations WHERE merchant_id=sqlc.arg(merchant_id)::uuid
 AND action_key=sqlc.arg(action_key)::text AND reserved_at<=sqlc.arg(day_since)::timestamptz;
