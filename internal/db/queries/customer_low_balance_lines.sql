-- billing.customer_low_balance_lines: a customer's own low-balance amount per
-- currency; with none it inherits the merchant's.

-- name: GetCustomerLowBalanceLine :one
SELECT * FROM billing.customer_low_balance_lines
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text;

-- name: ListCustomerLowBalanceLines :many
-- The named customers' lines, by currency: at most one row per customer and
-- registered currency, which bounds row_limit.
SELECT * FROM billing.customer_low_balance_lines
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
ORDER BY customer_id, currency
LIMIT sqlc.arg(row_limit)::int;

-- name: UpsertCustomerLowBalanceLine :exec
INSERT INTO billing.customer_low_balance_lines (merchant_id, customer_id, currency, amount, created_at, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text,
        sqlc.arg(amount)::bigint, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    amount = EXCLUDED.amount, updated_at = EXCLUDED.updated_at;

-- name: DeleteCustomerLowBalanceLine :exec
DELETE FROM billing.customer_low_balance_lines
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text;
