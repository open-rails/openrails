-- Declarative catalog definitions: the meter/rate-card sync behind catalog
-- applies. Rows follow the merchant's own declarations.

-- name: ListCatalogMeters :many
SELECT key,
       COALESCE(event_type, '')::text AS event_type,
       COALESCE(value_property, '')::text AS value_property,
       COALESCE(aggregation, '')::text AS aggregation,
       COALESCE(unit, '')::text AS unit,
       group_by
FROM billing.catalog_meters
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY key;

-- name: ListDefaultCatalogRateCards :many
SELECT rc.id, rc.created_at, COALESCE(p.key, '')::text AS product_key, rc.ordinal,
       COALESCE(rc.meter_key, '')::text AS meter_key, rc.payment_term, rc.filter,
       rc.allowance, rc.price
FROM billing.catalog_rate_cards rc
LEFT JOIN billing.products p ON p.merchant_id = rc.merchant_id AND p.id = rc.product_id
WHERE rc.merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: SyncCatalogMeter :exec
INSERT INTO billing.catalog_meters (merchant_id, key, event_type, value_property, aggregation, unit, group_by)
VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(meter_key)::text,
    NULLIF(sqlc.arg(event_type)::text, ''), NULLIF(sqlc.arg(value_property)::text, ''),
    NULLIF(sqlc.arg(aggregation)::text, ''), NULLIF(sqlc.arg(unit)::text, ''),
    sqlc.arg(group_by)::jsonb
)
ON CONFLICT (merchant_id, key) DO UPDATE SET event_type = EXCLUDED.event_type,
    value_property = EXCLUDED.value_property, aggregation = EXCLUDED.aggregation,
    unit = EXCLUDED.unit, group_by = EXCLUDED.group_by, updated_at = now()
WHERE sqlc.arg(overwrite)::boolean;

-- name: SyncCatalogRateCard :exec
INSERT INTO billing.catalog_rate_cards
    (merchant_id, product_id, ordinal, meter_key, payment_term, filter, allowance, price, id, created_at)
VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.narg(product_id)::uuid, sqlc.arg(ordinal)::bigint,
    NULLIF(sqlc.arg(meter_key)::text, ''), NULLIF(sqlc.arg(payment_term)::text, ''), sqlc.arg(filter)::jsonb,
    sqlc.narg(allowance)::jsonb, sqlc.arg(price)::jsonb,
    COALESCE(sqlc.narg(id)::uuid, uuidv7()), COALESCE(sqlc.narg(created_at)::timestamptz, now())
)
ON CONFLICT (merchant_id, product_id, ordinal) DO UPDATE SET meter_key = EXCLUDED.meter_key,
    payment_term = EXCLUDED.payment_term, filter = EXCLUDED.filter, allowance = EXCLUDED.allowance,
    price = EXCLUDED.price, updated_at = now()
WHERE sqlc.arg(overwrite)::boolean;

-- name: DeleteDefaultCatalogRateCard :exec
DELETE FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: DeleteCatalogMeter :exec
DELETE FROM billing.catalog_meters
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(meter_key)::text;
