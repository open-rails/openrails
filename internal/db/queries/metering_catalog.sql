-- Merchant-admin usage-meter catalog reads and lifecycle guards (#805).

-- name: LockUsageMeterKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: InsertUsageMeter :exec
INSERT INTO openrails.catalog_meters
    (merchant_id, key, event_type, value_property, aggregation, unit, group_by)
VALUES (
    sqlc.arg(merchant_id),
    sqlc.arg(meter_key),
    NULLIF(sqlc.arg(event_type)::text, ''),
    NULLIF(sqlc.arg(value_property)::text, ''),
    sqlc.arg(aggregation)::text,
    NULLIF(sqlc.arg(unit)::text, ''),
    sqlc.arg(group_by)::jsonb
);

-- name: UpdateUsageMeter :exec
UPDATE openrails.catalog_meters
SET event_type = NULLIF(sqlc.arg(event_type)::text, ''),
    value_property = NULLIF(sqlc.arg(value_property)::text, ''),
    aggregation = sqlc.arg(aggregation)::text,
    unit = NULLIF(sqlc.arg(unit)::text, ''),
    group_by = sqlc.arg(group_by)::jsonb,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND key = sqlc.arg(meter_key);

-- name: UpsertPayerUsageRateCard :exec
INSERT INTO openrails.catalog_rate_cards
    (merchant_id, product_id, customer_id, ordinal, meter_key, payment_term, filter, allowance, price)
VALUES (
    sqlc.arg(merchant_id), NULL, sqlc.arg(customer_id)::uuid, 1,
    sqlc.arg(meter_key)::text, 'in_arrears', '{}'::jsonb,
    sqlc.narg(allowance)::jsonb, sqlc.arg(price)::jsonb
)
ON CONFLICT (merchant_id, customer_id, meter_key)
    WHERE meter_key IS NOT NULL AND customer_id IS NOT NULL
DO UPDATE SET product_id = NULL,
    filter = '{}'::jsonb,
    allowance = EXCLUDED.allowance,
    price = EXCLUDED.price,
    updated_at = now();

-- name: LockUsageRateCardProduct :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 1));

-- name: UsageRateCardCurrencyConflict :one
SELECT EXISTS (
    SELECT 1
    FROM openrails.catalog_rate_cards
    WHERE merchant_id = sqlc.arg(merchant_id)
      AND meter_key = sqlc.arg(meter_key)::text
      AND customer_id IS NOT NULL
      AND upper(COALESCE(price ->> 'currency', '')) <> sqlc.arg(currency)
);

-- name: UpsertDefaultUsageRateCard :exec
INSERT INTO openrails.catalog_rate_cards
    (merchant_id, product_id, ordinal, meter_key, payment_term, filter, allowance, price)
VALUES (
    sqlc.arg(merchant_id),
    sqlc.arg(product_id)::uuid,
    COALESCE(
        (SELECT ordinal FROM openrails.catalog_rate_cards
         WHERE merchant_id = sqlc.arg(merchant_id)
           AND product_id = sqlc.arg(product_id)::uuid
           AND meter_key = sqlc.arg(meter_key)::text
           AND customer_id IS NULL),
        (SELECT MAX(ordinal) + 1 FROM openrails.catalog_rate_cards
         WHERE merchant_id = sqlc.arg(merchant_id)
           AND product_id = sqlc.arg(product_id)::uuid
           AND customer_id IS NULL),
        1
    ),
    sqlc.arg(meter_key)::text, 'in_arrears', sqlc.arg(filter)::jsonb,
    sqlc.narg(allowance)::jsonb, sqlc.arg(price)::jsonb
)
ON CONFLICT (merchant_id, meter_key)
    WHERE meter_key IS NOT NULL AND customer_id IS NULL
DO UPDATE SET product_id = EXCLUDED.product_id,
    ordinal = EXCLUDED.ordinal,
    filter = EXCLUDED.filter,
    allowance = EXCLUDED.allowance,
    price = EXCLUDED.price,
    updated_at = now();

-- name: GetDefaultUsageRateCardStateForUpdate :one
SELECT filter, allowance, price
FROM openrails.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)
  AND meter_key = sqlc.arg(meter_key)::text
  AND customer_id IS NULL
FOR UPDATE;

-- name: ListUsageRateCardPricesForUpdate :many
SELECT customer_id, price
FROM openrails.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)
  AND meter_key = sqlc.arg(meter_key)::text
FOR UPDATE;

-- name: GetUsageRateCardAllowanceDependencyCurrencies :one
SELECT COALESCE(
    array_agg(DISTINCT upper(COALESCE(price ->> 'currency', ''))),
    ARRAY[]::text[]
)::text[] AS currencies
FROM openrails.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)
  AND allowance ->> 'accrue_from' = sqlc.arg(meter_key)::text;

-- The meter row is already locked by the caller, which serializes every rate-card
-- mutation for this meter; PostgreSQL does not permit FOR UPDATE on aggregates.
-- name: GetDefaultUsageRateCardDeleteState :one
SELECT count(*) FILTER (WHERE customer_id IS NULL) > 0 AS default_exists,
       count(*) FILTER (WHERE customer_id IS NOT NULL) AS override_count
FROM openrails.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)
  AND meter_key = sqlc.arg(meter_key)::text;

-- name: DeleteDefaultUsageRateCard :exec
DELETE FROM openrails.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)
  AND meter_key = sqlc.arg(meter_key)::text
  AND customer_id IS NULL;

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
