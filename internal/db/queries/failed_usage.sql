-- billing.failed_usage_windows: failed usage per customer and configured
-- window, written in the transaction that records the failed usage event.

-- name: GetFailedUsageWindowAmount :one
SELECT COALESCE((
    SELECT amount FROM billing.failed_usage_windows
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
      AND currency = sqlc.arg(currency)::text AND invoker = sqlc.arg(invoker)::text
      AND window_key = sqlc.arg(window_key)::text AND window_start = sqlc.arg(window_start)::timestamptz
), 0)::bigint AS amount;

-- name: AddFailedUsageWindow :exec
INSERT INTO billing.failed_usage_windows (merchant_id, customer_id, currency, invoker, window_key, window_start, window_end, amount)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text, sqlc.arg(invoker)::text,
        sqlc.arg(window_key)::text, sqlc.arg(window_start)::timestamptz, sqlc.arg(window_end)::timestamptz, sqlc.arg(amount)::bigint)
ON CONFLICT (merchant_id, customer_id, currency, invoker, window_key, window_start)
DO UPDATE SET amount = billing.failed_usage_windows.amount + EXCLUDED.amount;

-- Ended windows go a bounded batch at a time, on the next write.
-- name: PruneFailedUsageWindows :execrows
DELETE FROM billing.failed_usage_windows w
WHERE (w.merchant_id, w.customer_id, w.currency, w.invoker, w.window_key, w.window_start) IN (
    SELECT e.merchant_id, e.customer_id, e.currency, e.invoker, e.window_key, e.window_start
    FROM billing.failed_usage_windows e
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid AND e.window_end <= sqlc.arg(now)::timestamptz
    LIMIT 100);
