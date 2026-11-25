-- Merchant-admin usage-meter catalog reads and lifecycle guards (#805).

-- name: LockUsageMeterKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: UpsertUsageMeter :exec
INSERT INTO billing.catalog_meters
    (merchant_id, key, event_type, value_property, aggregation, unit, group_by)
VALUES (
    sqlc.arg(merchant_id), sqlc.arg(meter_key),
    NULLIF(sqlc.arg(event_type)::text, ''), NULLIF(sqlc.arg(value_property)::text, ''),
    NULLIF(sqlc.arg(aggregation)::text, ''), NULLIF(sqlc.arg(unit)::text, ''),
    sqlc.arg(group_by)::jsonb
)
ON CONFLICT (merchant_id, key) DO UPDATE SET event_type = EXCLUDED.event_type,
    value_property = EXCLUDED.value_property, aggregation = EXCLUDED.aggregation,
    unit = EXCLUDED.unit, group_by = EXCLUDED.group_by, updated_at = now();

-- name: LockUsageRateCardProduct :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 1));

-- name: UpsertUsageRateCard :exec
INSERT INTO billing.catalog_rate_cards
    (merchant_id, product_id, ordinal, meter_key, payment_term, filter, allowance, price, id, created_at)
VALUES (
    sqlc.arg(merchant_id), sqlc.narg(product_id)::uuid,
    COALESCE(
        NULLIF(sqlc.arg(ordinal)::bigint, 0),
        (SELECT ordinal FROM billing.catalog_rate_cards
         WHERE merchant_id = sqlc.arg(merchant_id)
           AND product_id = sqlc.narg(product_id)::uuid
           AND meter_key = sqlc.arg(meter_key)::text),
        (SELECT MAX(ordinal) + 1 FROM billing.catalog_rate_cards
         WHERE merchant_id = sqlc.arg(merchant_id)
           AND product_id = sqlc.narg(product_id)::uuid),
        1
    ),
    NULLIF(sqlc.arg(meter_key)::text, ''), NULLIF(sqlc.arg(payment_term)::text, ''),
    sqlc.arg(filter)::jsonb, sqlc.narg(allowance)::jsonb, sqlc.arg(price)::jsonb,
    COALESCE(sqlc.narg(id)::uuid,
        (SELECT id FROM billing.catalog_rate_cards
         WHERE merchant_id = sqlc.arg(merchant_id)
           AND meter_key = sqlc.arg(meter_key)::text
           AND sqlc.arg(ordinal)::bigint = 0),
        uuidv7()),
    COALESCE(sqlc.narg(created_at)::timestamptz, now())
)
ON CONFLICT (merchant_id, id) DO UPDATE SET product_id = EXCLUDED.product_id,
    ordinal = EXCLUDED.ordinal, meter_key = EXCLUDED.meter_key,
    payment_term = EXCLUDED.payment_term, filter = EXCLUDED.filter,
    allowance = EXCLUDED.allowance, price = EXCLUDED.price, updated_at = now();

-- name: GetDefaultUsageRateCardStateForUpdate :one
SELECT filter, allowance, price
FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)
  AND meter_key = sqlc.arg(meter_key)::text
FOR UPDATE;

-- The meter row is already locked by the caller, which serializes every rate-card
-- mutation for this meter.
-- name: DefaultUsageRateCardExists :one
SELECT EXISTS (
    SELECT 1
    FROM billing.catalog_rate_cards
    WHERE merchant_id = sqlc.arg(merchant_id)
      AND meter_key = sqlc.arg(meter_key)::text
);

-- name: DeleteDefaultUsageRateCard :exec
DELETE FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)
  AND meter_key = sqlc.arg(meter_key)::text;

-- name: ListUsageMetersWithCatalog :many
SELECT meter.key,
       COALESCE(meter.event_type, '') AS event_type,
       COALESCE(NULLIF(meter.event_type, ''), meter.key) AS effective_event_type,
       COALESCE(meter.value_property, '') AS value_property,
       COALESCE(meter.aggregation, '') AS aggregation,
       COALESCE(meter.unit, '') AS unit,
       meter.group_by,
       meter.revision,
       meter.created_at,
       meter.updated_at,
       card.id AS card_id,
       card.product_id,
       product.key AS product_key,
       card.filter,
       card.price,
       card.allowance,
       card.created_at AS card_created_at,
       card.updated_at AS card_updated_at
FROM billing.catalog_meters meter
LEFT JOIN billing.catalog_rate_cards card
  ON card.merchant_id = meter.merchant_id
 AND card.meter_key = meter.key
LEFT JOIN billing.products product
  ON product.merchant_id = card.merchant_id
 AND product.id = card.product_id
WHERE meter.merchant_id = sqlc.arg(merchant_id)
  AND (sqlc.narg(after_key)::text IS NULL OR meter.key > sqlc.narg(after_key)::text)
ORDER BY meter.key
LIMIT sqlc.arg(fetch_limit)::int;

-- name: GetUsageMeterWithCatalog :one
SELECT meter.key,
       COALESCE(meter.event_type, '') AS event_type,
       COALESCE(NULLIF(meter.event_type, ''), meter.key) AS effective_event_type,
       COALESCE(meter.value_property, '') AS value_property,
       COALESCE(meter.aggregation, '') AS aggregation,
       COALESCE(meter.unit, '') AS unit,
       meter.group_by,
       meter.revision,
       meter.created_at,
       meter.updated_at,
       card.id AS card_id,
       card.product_id,
       product.key AS product_key,
       card.filter,
       card.price,
       card.allowance,
       card.created_at AS card_created_at,
       card.updated_at AS card_updated_at
FROM billing.catalog_meters meter
LEFT JOIN billing.catalog_rate_cards card
  ON card.merchant_id = meter.merchant_id
 AND card.meter_key = meter.key
LEFT JOIN billing.products product
  ON product.merchant_id = card.merchant_id
 AND product.id = card.product_id
WHERE meter.merchant_id = sqlc.arg(merchant_id)
  AND meter.key = sqlc.arg(meter_key);

-- name: GetUsageMeterForUpdate :one
SELECT key,
       COALESCE(event_type, '') AS event_type,
       COALESCE(value_property, '') AS value_property,
       COALESCE(aggregation, '') AS aggregation,
       COALESCE(unit, '') AS unit,
       group_by
FROM billing.catalog_meters
WHERE merchant_id = sqlc.arg(merchant_id)
  AND key = sqlc.arg(meter_key)
FOR UPDATE;

-- name: GetActiveMeteringProductForShare :one
SELECT id
FROM billing.products
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(product_id)
  AND NOT archived
FOR SHARE;
