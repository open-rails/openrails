-- name: ListCatalogStateProducts :many
-- Catalog objects as one state, read under the catalog lock before and after
-- a catalog write: every product with its keys and revision.
SELECT id, key, display_name, COALESCE(description, '')::text AS description,
       COALESCE((SELECT jsonb_agg(pe.entitlement ORDER BY pe.entitlement) FROM billing.product_entitlements pe WHERE pe.merchant_id = products.merchant_id AND pe.product_id = products.id AND pe.removed_at IS NULL), '[]'::jsonb)::jsonb AS entitlements,
       credit_grant, tier_group, tier_rank, ownership, archived, revision
FROM billing.products
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY key;

-- name: ListCatalogStatePrices :many
-- Every version of every price key, with its PSP links and the key's revision.
SELECT p.id, p.product_id, p.key, p.revision AS version, p.amount, p.currency, p.access_duration_hours, p.billing_interval_hours,
       p.trial_unit_amount, p.trial_duration_hours, p.customer_amount, p.quantity, p.archived,
       COALESCE((
           SELECT jsonb_object_agg(COALESCE(psp.key, psp.id::text), binding.configuration || jsonb_strip_nulls(jsonb_build_object(
               'psp_id', psp.id::text, 'rail', psp.rail, 'plan_id', binding.plan_id, 'price_id', binding.price_ref,
               'recurring_billing_option_id', binding.recurring_billing_option_id, 'plan_pda', binding.plan_pda,
               'flex_id', binding.flex_id)))
           FROM billing.price_psp_bindings binding
           JOIN billing.psps psp ON psp.id = binding.psp_id AND psp.merchant_id = binding.merchant_id
           WHERE binding.price_id = p.id AND binding.merchant_id = p.merchant_id
       ), '{}'::jsonb)::jsonb AS psp_links,
       COALESCE(k.revision, 1)::bigint AS key_revision
FROM billing.prices p
LEFT JOIN billing.price_keys k ON k.merchant_id = p.merchant_id AND k.product_id = p.product_id AND k.key = p.key
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY p.product_id, p.key, p.revision;

-- name: ListCatalogMeterRevisions :many
SELECT key, revision
FROM billing.catalog_meters
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY key;

-- name: ListCatalogFieldOwners :many
SELECT object, key, price_key, field, manager, actor, set_at
FROM billing.catalog_field_owners
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY object, key, price_key, field, manager;

-- name: UpsertCatalogFieldOwners :exec
-- One manager takes the fields; it is now the one that last set them.
INSERT INTO billing.catalog_field_owners (merchant_id, object, key, price_key, field, manager, actor, set_at)
SELECT sqlc.arg(merchant_id)::uuid, f.object, f.key, f.price_key, f.field, sqlc.arg(manager)::text, sqlc.arg(actor)::text, sqlc.arg(set_at)::timestamptz
FROM unnest(sqlc.arg(objects)::text[], sqlc.arg(keys)::text[], sqlc.arg(price_keys)::text[], sqlc.arg(fields)::text[]) AS f(object, key, price_key, field)
ON CONFLICT (merchant_id, object, key, price_key, field, manager) DO UPDATE SET actor = EXCLUDED.actor, set_at = EXCLUDED.set_at;

-- name: DeleteCatalogFieldOwners :exec
-- The manager no longer owns the fields.
DELETE FROM billing.catalog_field_owners o
USING unnest(sqlc.arg(objects)::text[], sqlc.arg(keys)::text[], sqlc.arg(price_keys)::text[], sqlc.arg(fields)::text[]) AS f(object, key, price_key, field)
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.manager = sqlc.arg(manager)::text
  AND o.object = f.object AND o.key = f.key AND o.price_key = f.price_key AND o.field = f.field;

-- name: DeleteCatalogObjectOwners :exec
-- A deleted object has no fields to own.
DELETE FROM billing.catalog_field_owners o
USING unnest(sqlc.arg(objects)::text[], sqlc.arg(keys)::text[], sqlc.arg(price_keys)::text[]) AS f(object, key, price_key)
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid
  AND o.object = f.object AND o.key = f.key AND o.price_key = f.price_key;

-- name: StepMeterRevisions :exec
-- A meter's rate card is part of the meter.
UPDATE billing.catalog_meters
SET revision = revision + 1, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND key = ANY(sqlc.arg(keys)::text[]);

-- name: ListPriceKeyRevisions :many
SELECT p.id, k.revision
FROM billing.prices p
JOIN billing.price_keys k ON k.merchant_id = p.merchant_id AND k.product_id = p.product_id AND k.key = p.key
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.id = ANY(sqlc.arg(price_ids)::uuid[]);
