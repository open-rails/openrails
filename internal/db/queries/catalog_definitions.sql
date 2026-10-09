-- Declarative catalog definitions: the meter/rate-card sync behind catalog
-- applies and the manifest dump. Rows follow the merchant's own declarations.

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
WHERE rc.merchant_id = sqlc.arg(merchant_id)::uuid AND rc.customer_id IS NULL;

-- name: ListCatalogProductRateCards :many
SELECT rc.product_id::uuid AS product_id, rc.ordinal, rc.meter_key, rc.payment_term, rc.filter, rc.allowance, rc.price
FROM billing.catalog_rate_cards rc
JOIN billing.products p ON p.merchant_id = rc.merchant_id AND p.id = rc.product_id
WHERE rc.merchant_id = sqlc.arg(merchant_id)::uuid
  AND rc.customer_id IS NULL
ORDER BY rc.product_id, rc.ordinal;

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
    NULLIF(sqlc.arg(meter_key)::text, ''), sqlc.arg(payment_term)::text, sqlc.arg(filter)::jsonb,
    sqlc.narg(allowance)::jsonb, sqlc.arg(price)::jsonb,
    COALESCE(sqlc.narg(id)::uuid, gen_random_uuid()), COALESCE(sqlc.narg(created_at)::timestamptz, now())
)
ON CONFLICT (merchant_id, product_id, ordinal) DO UPDATE SET meter_key = EXCLUDED.meter_key,
    payment_term = EXCLUDED.payment_term, filter = EXCLUDED.filter, allowance = EXCLUDED.allowance,
    price = EXCLUDED.price, updated_at = now()
WHERE sqlc.arg(overwrite)::boolean;

-- name: DeleteDefaultCatalogRateCard :exec
DELETE FROM billing.catalog_rate_cards
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND customer_id IS NULL;

-- name: DeleteCatalogMeter :exec
DELETE FROM billing.catalog_meters
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(meter_key)::text;

-- name: GetCatalogRevisionForShare :one
SELECT catalog_revision FROM billing.merchants WHERE id = sqlc.arg(merchant_id)::uuid FOR SHARE;

-- name: ListLiveCatalogProducts :many
SELECT id, key, display_name, COALESCE(description, '')::text AS description,
       COALESCE((SELECT jsonb_agg(pe.entitlement ORDER BY pe.entitlement) FROM billing.product_entitlements pe WHERE pe.merchant_id = products.merchant_id AND pe.product_id = products.id AND pe.removed_at IS NULL), '[]'::jsonb)::jsonb AS entitlements, credit_grant,
       tier_group, tier_rank, archived
FROM billing.products
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND NOT archived
ORDER BY COALESCE(tier_group, ''), tier_rank, key;

-- name: ListLiveCatalogPricesWithPSPLinks :many
SELECT p.product_id, p.key, p.amount, p.currency, p.access_duration_hours, p.billing_interval_hours,
       p.trial_unit_amount, p.trial_duration_hours, p.customer_amount,
       COALESCE((
           SELECT jsonb_object_agg(COALESCE(psp.key, psp.id::text), binding.configuration || jsonb_strip_nulls(jsonb_build_object(
               'psp_id', psp.id::text, 'rail', psp.rail, 'plan_id', binding.plan_id, 'price_id', binding.price_ref,
               'recurring_billing_option_id', binding.recurring_billing_option_id, 'plan_pda', binding.plan_pda,
               'flex_id', binding.flex_id)))
           FROM billing.price_psp_bindings binding
           JOIN billing.psps psp ON psp.id = binding.psp_id AND psp.merchant_id = binding.merchant_id
           WHERE binding.price_id = p.id AND binding.merchant_id = p.merchant_id
       ), '{}'::jsonb)::jsonb AS psp_links,
       p.archived
FROM billing.prices p
JOIN billing.products product ON product.merchant_id = p.merchant_id AND product.id = p.product_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND NOT p.archived
ORDER BY p.product_id, p.amount, p.currency, p.key;
