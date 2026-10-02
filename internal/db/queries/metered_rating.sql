-- Metered rating: the rate cards that apply to a payer, usage aggregation for
-- a rated window, and the per-period accrual watermark.

-- A payer-scoped card (customer_id set) later replaces the default for its meter.
-- name: ListPayerArrearsRateCards :many
SELECT rc.id,
       rc.meter_key::text AS meter_key,
       (rc.customer_id IS NOT NULL)::boolean AS payer_scoped,
       COALESCE(NULLIF(cm.event_type, ''), cm.key)::text AS event_type,
       COALESCE(NULLIF(cm.value_property, ''), cm.key)::text AS value_property,
       COALESCE(NULLIF(cm.aggregation, ''), 'sum')::text AS aggregation,
       COALESCE(cm.group_by, '{}'::jsonb)::jsonb AS group_by,
       COALESCE(rc.filter, '{}'::jsonb)::jsonb AS filter,
       rc.allowance,
       rc.price
FROM openrails.catalog_rate_cards rc
JOIN openrails.catalog_meters cm ON cm.merchant_id = rc.merchant_id AND cm.key = rc.meter_key
WHERE rc.merchant_id = sqlc.arg(merchant_id)::uuid
  AND rc.meter_key IS NOT NULL
  AND (rc.customer_id IS NULL OR rc.customer_id = sqlc.arg(customer_id)::uuid)
  AND lower(COALESCE(rc.price ->> 'currency', sqlc.arg(currency)::text)) = lower(sqlc.arg(currency)::text)
  AND rc.payment_term = 'in_arrears'
ORDER BY rc.ordinal;

-- filter_rules is [{property_key, allowed_values}]; every rule must admit the event.
-- name: SumCatalogUsageByDimension :many
SELECT COALESCE(NULLIF(ue.metadata ->> sqlc.arg(group_property)::text, ''),
                NULLIF(ue.dimensions ->> sqlc.arg(group_property)::text, ''), '')::text AS dim_value,
       COALESCE(SUM(
           CASE WHEN sqlc.arg(aggregation)::text = 'count' THEN 1
                ELSE COALESCE((ue.dimensions ->> sqlc.arg(value_key)::text)::bigint,
                              (ue.metadata ->> sqlc.arg(value_key)::text)::bigint, 0)
           END), 0)::bigint AS quantity
FROM openrails.usage_events ue
WHERE ue.merchant_id = sqlc.arg(merchant_id)::uuid
  AND ue.customer_id = sqlc.arg(customer_id)::uuid
  AND ue.currency = sqlc.arg(currency)::text
  AND ue.event_type = sqlc.arg(event_type)::text
  AND ue.pricing_authority = 'catalog'
  AND ue.occurred_at >= sqlc.arg(occurred_from)::timestamptz
  AND ue.occurred_at < sqlc.arg(occurred_to)::timestamptz
  AND NOT EXISTS (
      SELECT 1
      FROM jsonb_to_recordset(sqlc.arg(filter_rules)::jsonb) AS filter_rule(property_key text, allowed_values jsonb)
      WHERE NOT EXISTS (
          SELECT 1
          FROM jsonb_array_elements_text(filter_rule.allowed_values) AS allowed_value(value)
          WHERE allowed_value.value = COALESCE(
              NULLIF(ue.metadata ->> filter_rule.property_key, ''),
              NULLIF(ue.dimensions ->> filter_rule.property_key, ''), '')))
GROUP BY 1;

-- name: SumCatalogUsageByDimensionResource :many
SELECT COALESCE(NULLIF(ue.metadata ->> sqlc.arg(dimension_property)::text, ''),
                NULLIF(ue.dimensions ->> sqlc.arg(dimension_property)::text, ''), '')::text AS dim_value,
       COALESCE(NULLIF(ue.metadata ->> sqlc.arg(resource_property)::text, ''),
                NULLIF(ue.dimensions ->> sqlc.arg(resource_property)::text, ''), '')::text AS resource_id,
       COALESCE(SUM(
           CASE WHEN sqlc.arg(aggregation)::text = 'count' THEN 1
                ELSE COALESCE((ue.dimensions ->> sqlc.arg(value_key)::text)::bigint,
                              (ue.metadata ->> sqlc.arg(value_key)::text)::bigint, 0)
           END), 0)::bigint AS quantity
FROM openrails.usage_events ue
WHERE ue.merchant_id = sqlc.arg(merchant_id)::uuid
  AND ue.customer_id = sqlc.arg(customer_id)::uuid
  AND ue.currency = sqlc.arg(currency)::text
  AND ue.event_type = sqlc.arg(event_type)::text
  AND ue.pricing_authority = 'catalog'
  AND ue.occurred_at >= sqlc.arg(occurred_from)::timestamptz
  AND ue.occurred_at < sqlc.arg(occurred_to)::timestamptz
  AND NOT EXISTS (
      SELECT 1
      FROM jsonb_to_recordset(sqlc.arg(filter_rules)::jsonb) AS filter_rule(property_key text, allowed_values jsonb)
      WHERE NOT EXISTS (
          SELECT 1
          FROM jsonb_array_elements_text(filter_rule.allowed_values) AS allowed_value(value)
          WHERE allowed_value.value = COALESCE(
              NULLIF(ue.metadata ->> filter_rule.property_key, ''),
              NULLIF(ue.dimensions ->> filter_rule.property_key, ''), '')))
GROUP BY 1, 2;

-- ON CONFLICT DO UPDATE takes the row lock, serializing concurrent sweeps.
-- name: LockMeteredRatingWatermark :one
INSERT INTO openrails.metered_rating_watermarks (
    merchant_id, customer_id, currency, source, period_from, rated_through, accrued_amount, created_at, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text, sqlc.arg(source)::text,
    sqlc.arg(period_from)::timestamptz, sqlc.arg(period_from)::timestamptz, 0, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
)
ON CONFLICT (merchant_id, customer_id, currency, source, period_from)
DO UPDATE SET updated_at = openrails.metered_rating_watermarks.updated_at
RETURNING accrued_amount;

-- name: AdvanceMeteredRatingWatermark :exec
UPDATE openrails.metered_rating_watermarks
SET rated_through = GREATEST(rated_through, sqlc.arg(rated_through)::timestamptz),
    accrued_amount = accrued_amount + sqlc.arg(accrued_delta)::bigint,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND source = sqlc.arg(source)::text
  AND period_from = sqlc.arg(period_from)::timestamptz;
