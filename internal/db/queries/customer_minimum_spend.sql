-- openrails.customer_minimum_spend (#643): per-customer per-currency minimum-spend
-- commitment that trues-up at periodic invoice close.

-- name: GetCustomerMinimumSpend :one
SELECT amount_micros
FROM openrails.customer_minimum_spend
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text;

-- name: UpsertCustomerMinimumSpend :exec
INSERT INTO openrails.customer_minimum_spend (
    merchant_id, customer_id, currency, amount_micros, created_at, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text,
    sqlc.arg(amount_micros)::bigint, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE
    SET amount_micros = EXCLUDED.amount_micros, updated_at = EXCLUDED.updated_at;

-- name: DeleteCustomerMinimumSpend :exec
DELETE FROM openrails.customer_minimum_spend
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text;
