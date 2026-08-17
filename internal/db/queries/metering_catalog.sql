-- Merchant-admin usage-meter catalog reads and lifecycle guards (#805).

-- name: CountUsageMeters :one
SELECT count(*)
FROM openrails.catalog_meters
WHERE merchant_id = sqlc.arg(merchant_id);

-- name: ListUsageMetersWithCatalog :many
WITH activity AS (
    SELECT event_type, count(*) AS event_count, max(occurred_at) AS last_event_at
    FROM openrails.usage_events
    WHERE merchant_id = sqlc.arg(merchant_id)
    GROUP BY event_type
), override_counts AS (
    SELECT meter_key, count(*) AS override_count
    FROM openrails.catalog_rate_cards
    WHERE merchant_id = sqlc.arg(merchant_id) AND customer_id IS NOT NULL
    GROUP BY meter_key
)
SELECT meter.key,
       COALESCE(meter.event_type, '') AS event_type,
       COALESCE(NULLIF(meter.event_type, ''), meter.key) AS effective_event_type,
       COALESCE(meter.value_property, '') AS value_property,
       COALESCE(meter.aggregation, '') AS aggregation,
       COALESCE(meter.unit, '') AS unit,
       meter.group_by,
       meter.created_at,
       meter.updated_at,
       COALESCE(override_counts.override_count, 0) AS override_count,
       COALESCE(activity.event_count, 0) > 0 AS has_activity,
       activity.last_event_at,
       card.id AS card_id,
       card.product_id,
       product.key AS product_key,
       card.filter,
       card.price,
       card.allowance,
       card.created_at AS card_created_at,
       card.updated_at AS card_updated_at
FROM openrails.catalog_meters meter
LEFT JOIN activity
  ON activity.event_type = COALESCE(NULLIF(meter.event_type, ''), meter.key)
LEFT JOIN override_counts
  ON override_counts.meter_key = meter.key
LEFT JOIN openrails.catalog_rate_cards card
  ON card.merchant_id = meter.merchant_id
 AND card.meter_key = meter.key
 AND card.customer_id IS NULL
LEFT JOIN openrails.products product
  ON product.merchant_id = card.merchant_id
 AND product.id = card.product_id
WHERE meter.merchant_id = sqlc.arg(merchant_id)
ORDER BY meter.key
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: GetUsageMeterWithCatalog :one
WITH activity AS (
    SELECT event_type, count(*) AS event_count, max(occurred_at) AS last_event_at
    FROM openrails.usage_events
    WHERE merchant_id = sqlc.arg(merchant_id)
    GROUP BY event_type
), override_counts AS (
    SELECT meter_key, count(*) AS override_count
    FROM openrails.catalog_rate_cards
    WHERE merchant_id = sqlc.arg(merchant_id) AND customer_id IS NOT NULL
    GROUP BY meter_key
)
SELECT meter.key,
       COALESCE(meter.event_type, '') AS event_type,
       COALESCE(NULLIF(meter.event_type, ''), meter.key) AS effective_event_type,
       COALESCE(meter.value_property, '') AS value_property,
       COALESCE(meter.aggregation, '') AS aggregation,
       COALESCE(meter.unit, '') AS unit,
       meter.group_by,
       meter.created_at,
       meter.updated_at,
       COALESCE(override_counts.override_count, 0) AS override_count,
       COALESCE(activity.event_count, 0) > 0 AS has_activity,
       activity.last_event_at,
       card.id AS card_id,
       card.product_id,
       product.key AS product_key,
       card.filter,
       card.price,
       card.allowance,
       card.created_at AS card_created_at,
       card.updated_at AS card_updated_at
FROM openrails.catalog_meters meter
LEFT JOIN activity
  ON activity.event_type = COALESCE(NULLIF(meter.event_type, ''), meter.key)
LEFT JOIN override_counts
  ON override_counts.meter_key = meter.key
LEFT JOIN openrails.catalog_rate_cards card
  ON card.merchant_id = meter.merchant_id
 AND card.meter_key = meter.key
 AND card.customer_id IS NULL
LEFT JOIN openrails.products product
  ON product.merchant_id = card.merchant_id
 AND product.id = card.product_id
WHERE meter.merchant_id = sqlc.arg(merchant_id)
  AND meter.key = sqlc.arg(meter_key);

-- name: UsageMeterExists :one
SELECT EXISTS (
    SELECT 1
    FROM openrails.catalog_meters
    WHERE merchant_id = sqlc.arg(merchant_id)
      AND key = sqlc.arg(meter_key)
);

-- name: CountUsageMeterOverrides :one
SELECT count(*)
FROM openrails.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)
  AND meter_key = sqlc.arg(meter_key)::text
  AND customer_id IS NOT NULL;

-- name: ListUsageMeterOverrides :many
SELECT card.customer_id,
       COALESCE(customer.subject, '') AS subject,
       COALESCE((
           SELECT BTRIM(subscription.user_email)
           FROM openrails.subscriptions subscription
           WHERE subscription.merchant_id = card.merchant_id
             AND subscription.customer_id = card.customer_id
             AND subscription.deleted_at IS NULL
             AND NULLIF(BTRIM(subscription.user_email), '') IS NOT NULL
           ORDER BY subscription.created_at DESC, subscription.id DESC
           LIMIT 1
       ), '') AS email,
       card.price,
       card.allowance,
       card.created_at,
       card.updated_at
FROM openrails.catalog_rate_cards card
JOIN openrails.customers customer
  ON customer.id = card.customer_id
WHERE card.merchant_id = sqlc.arg(merchant_id)
  AND card.meter_key = sqlc.arg(meter_key)::text
  AND card.customer_id IS NOT NULL
ORDER BY card.updated_at DESC, card.customer_id
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: LockUsageEventsForMeterCorrection :exec
LOCK TABLE openrails.usage_events IN SHARE ROW EXCLUSIVE MODE;

-- name: UsageEventsExistForTypes :one
SELECT EXISTS (
    SELECT 1
    FROM openrails.usage_events
    WHERE merchant_id = sqlc.arg(merchant_id)
      AND event_type = ANY(sqlc.arg(event_types)::text[])
);

-- name: GetUsageMeterForUpdate :one
SELECT key,
       COALESCE(event_type, '') AS event_type,
       COALESCE(value_property, '') AS value_property,
       COALESCE(aggregation, '') AS aggregation,
       COALESCE(unit, '') AS unit,
       group_by
FROM openrails.catalog_meters
WHERE merchant_id = sqlc.arg(merchant_id)
  AND key = sqlc.arg(meter_key)
FOR UPDATE;

-- name: GetDefaultUsageRateCardPriceForUpdate :one
SELECT price
FROM openrails.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)
  AND meter_key = sqlc.arg(meter_key)::text
  AND customer_id IS NULL
FOR UPDATE;

-- name: GetActiveMeteringProductForShare :one
SELECT id
FROM openrails.products
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(product_id)
  AND NOT archived
FOR SHARE;
