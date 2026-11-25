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

-- name: DeleteDefaultCatalogRateCard :exec
DELETE FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: DeleteCatalogMeter :exec
DELETE FROM billing.catalog_meters
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(meter_key)::text;
