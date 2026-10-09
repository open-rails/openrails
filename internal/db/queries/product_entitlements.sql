-- billing.product_entitlements: the keys each product grants, with history.

-- name: ListLiveProductEntitlements :many
-- The current keys of the given products, in byte order.
SELECT pe.product_id, pe.entitlement::text AS entitlement
FROM billing.product_entitlements pe
WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid
  AND pe.product_id = ANY(sqlc.arg(product_ids)::uuid[])
  AND pe.removed_at IS NULL
ORDER BY pe.product_id, pe.entitlement;

-- name: AddProductEntitlements :many
-- Opens a row for each key the product does not grant now.
INSERT INTO billing.product_entitlements (merchant_id, product_id, entitlement, added_at, added_by)
SELECT sqlc.arg(merchant_id)::uuid, sqlc.arg(product_id)::uuid, k.key, sqlc.arg(at)::timestamptz, sqlc.arg(actor)::text
FROM unnest(sqlc.arg(entitlements)::text[]) AS k(key)
ON CONFLICT (merchant_id, product_id, entitlement) WHERE removed_at IS NULL DO NOTHING
RETURNING entitlement::text AS entitlement;

-- name: RemoveProductEntitlements :many
-- Closes the product's live rows of the given keys.
UPDATE billing.product_entitlements pe
SET removed_at = GREATEST(sqlc.arg(at)::timestamptz, pe.added_at), removed_by = sqlc.arg(actor)::text
WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid
  AND pe.product_id = sqlc.arg(product_id)::uuid
  AND pe.removed_at IS NULL
  AND pe.entitlement = ANY(sqlc.arg(entitlements)::text[])
RETURNING pe.entitlement::text AS entitlement;

-- name: ReplaceEntitlement :many
-- Closes every live row of from_key and, unless to_key is NULL, opens to_key
-- on the same products. Returns the products that granted from_key.
WITH closed AS (
  UPDATE billing.product_entitlements pe
  SET removed_at = GREATEST(sqlc.arg(at)::timestamptz, pe.added_at), removed_by = sqlc.arg(actor)::text
  WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid
    AND pe.entitlement = sqlc.arg(from_key)::text
    AND pe.removed_at IS NULL
  RETURNING pe.product_id
), opened AS (
  INSERT INTO billing.product_entitlements (merchant_id, product_id, entitlement, added_at, added_by)
  SELECT sqlc.arg(merchant_id)::uuid, closed.product_id, sqlc.narg(to_key)::text, sqlc.arg(at)::timestamptz, sqlc.arg(actor)::text
  FROM closed
  WHERE sqlc.narg(to_key)::text IS NOT NULL
  ON CONFLICT (merchant_id, product_id, entitlement) WHERE removed_at IS NULL DO NOTHING
  RETURNING product_id
)
SELECT closed.product_id, p.key AS product_key, EXISTS (SELECT 1 FROM opened WHERE opened.product_id = closed.product_id) AS opened
FROM closed
JOIN billing.products p ON p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.id = closed.product_id
ORDER BY p.key;

-- name: StepProductRevisions :exec
-- A key edit is a product change.
UPDATE billing.products
SET revision = revision + 1, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(product_ids)::uuid[]);

-- name: ListRecurringBenefitOverlaps :many
-- Two recurring products granting one entitlement outside a shared tier
-- group: a customer could hold both and pay twice for that benefit.
SELECT a.entitlement, pa.key AS first_product, pb.key AS second_product
FROM billing.product_entitlements a
JOIN billing.product_entitlements b ON b.merchant_id = a.merchant_id AND b.entitlement = a.entitlement
 AND b.product_id > a.product_id AND b.removed_at IS NULL
JOIN billing.products pa ON pa.merchant_id = a.merchant_id AND pa.id = a.product_id
JOIN billing.products pb ON pb.merchant_id = b.merchant_id AND pb.id = b.product_id
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.removed_at IS NULL
  AND (pa.tier_group IS NULL OR pb.tier_group IS NULL OR pa.tier_group <> pb.tier_group)
  AND EXISTS (SELECT 1 FROM billing.prices x WHERE x.merchant_id = pa.merchant_id AND x.product_id = pa.id AND x.billing_interval_hours IS NOT NULL)
  AND EXISTS (SELECT 1 FROM billing.prices y WHERE y.merchant_id = pb.merchant_id AND y.product_id = pb.id AND y.billing_interval_hours IS NOT NULL)
ORDER BY a.entitlement, pa.key, pb.key
LIMIT 1000;
