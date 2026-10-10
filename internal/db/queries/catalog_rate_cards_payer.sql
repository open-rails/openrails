-- or#909: negotiated per-payer rate-card overrides (#798 storage) — the read
-- side. Writes stay in money/enterprise.go's SetUsageRateCard /
-- DeletePayerRateCard chokepoints.

-- name: ListRateOverrides :many
-- One keyset page of customers' overrides, by customer then meter; each
-- filter is optional.
SELECT customer_id, meter_key, allowance, price, revision, created_at, updated_at
FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id IS NOT NULL AND meter_key IS NOT NULL
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(meter_key)::text IS NULL OR meter_key = sqlc.narg(meter_key)::text)
  AND (sqlc.narg(after_customer)::uuid IS NULL
       OR (customer_id, meter_key) > (sqlc.narg(after_customer)::uuid, sqlc.narg(after_key)::text))
ORDER BY customer_id, meter_key
LIMIT sqlc.arg(fetch_limit)::int;

-- name: GetPayerRateCard :one
SELECT customer_id, meter_key, allowance, price, revision, created_at, updated_at
FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND meter_key = sqlc.arg(meter_key)::text;

-- name: DeletePayerRateCard :exec
DELETE FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND meter_key = sqlc.arg(meter_key)::text;
