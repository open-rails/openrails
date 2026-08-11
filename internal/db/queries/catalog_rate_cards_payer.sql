-- or#909: negotiated per-payer rate-card overrides (#798 storage) — the read
-- side. Writes stay in money/enterprise.go's SetUsageRateCard /
-- DeletePayerRateCard chokepoints.

-- name: ListPayerRateCards :many
-- A payer's negotiated overrides. Bounded by the payer's own contract (one
-- row per overridden meter), not by customer activity.
SELECT meter_key, product_id, allowance, price, created_at, updated_at
FROM openrails.catalog_rate_cards
WHERE merchant_id = $1 AND customer_id = $2 AND meter_key IS NOT NULL
ORDER BY meter_key;
