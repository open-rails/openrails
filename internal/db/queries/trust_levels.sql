-- name: ListCustomersTrustIn :many
-- The customers' trust tier in one currency, one row per customer named:
-- the explicit level and the paid totals the merchant's rules read, in one
-- read (billing.trust_tier; trust is merchantconfig.Settings.TrustParam).
SELECT c.customer_id::uuid AS customer_id, ms.tier AS explicit_tier,
    billing.trust_tier(sqlc.arg(trust)::jsonb, sqlc.arg(currency)::text, ms.tier, pt.net_paid_amount, pt.first_paid_at, sqlc.arg(now)::timestamptz)::text AS trust_level,
    COALESCE(pt.net_paid_amount, 0)::bigint AS net_paid_amount, pt.first_paid_at
FROM unnest(sqlc.arg(customer_ids)::uuid[]) AS c(customer_id)
LEFT JOIN billing.money_settings ms
  ON ms.merchant_id = sqlc.arg(merchant_id)::uuid AND ms.customer_id = c.customer_id AND ms.currency = sqlc.arg(currency)::text
LEFT JOIN billing.customer_paid_totals pt
  ON pt.merchant_id = sqlc.arg(merchant_id)::uuid AND pt.customer_id = c.customer_id AND pt.currency = sqlc.arg(currency)::text;

-- name: ListCustomersTrust :many
-- The customers' trust tiers in each currency they hold an explicit level
-- or paid totals in, by customer and currency.
SELECT k.customer_id, k.currency, ms.tier AS explicit_tier,
    billing.trust_tier(sqlc.arg(trust)::jsonb, k.currency, ms.tier, pt.net_paid_amount, pt.first_paid_at, sqlc.arg(now)::timestamptz)::text AS trust_level,
    COALESCE(pt.net_paid_amount, 0)::bigint AS net_paid_amount, pt.first_paid_at
FROM (
    SELECT s.customer_id, s.currency FROM billing.money_settings s
    WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.customer_id = ANY(sqlc.arg(customer_ids)::uuid[]) AND s.tier IS NOT NULL
    UNION
    SELECT t.customer_id, t.currency FROM billing.customer_paid_totals t
    WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
) k
LEFT JOIN billing.money_settings ms
  ON ms.merchant_id = sqlc.arg(merchant_id)::uuid AND ms.customer_id = k.customer_id AND ms.currency = k.currency
LEFT JOIN billing.customer_paid_totals pt
  ON pt.merchant_id = sqlc.arg(merchant_id)::uuid AND pt.customer_id = k.customer_id AND pt.currency = k.currency
ORDER BY k.customer_id, k.currency
LIMIT sqlc.arg(row_limit)::int;
