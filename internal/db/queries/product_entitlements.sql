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
