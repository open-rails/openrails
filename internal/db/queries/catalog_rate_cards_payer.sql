-- or#909: negotiated per-payer rate-card overrides (#798 storage) — the read
-- side. Writes stay in money/enterprise.go's SetUsageRateCard /
-- DeletePayerRateCard chokepoints.

-- name: ListPayerRateCards :many
-- One keyset page of a customer's overrides, by meter.
SELECT customer_id, meter_key, allowance, price, created_at, updated_at
FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND meter_key IS NOT NULL
  AND (sqlc.narg(after_key)::text IS NULL OR meter_key > sqlc.narg(after_key)::text)
ORDER BY meter_key
LIMIT sqlc.arg(fetch_limit)::int;

-- name: GetPayerRateCard :one
SELECT customer_id, meter_key, allowance, price, created_at, updated_at
FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND meter_key = sqlc.arg(meter_key)::text;

-- name: DeletePayerRateCard :exec
DELETE FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND meter_key = sqlc.arg(meter_key)::text;
