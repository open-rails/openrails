-- billing.event_store: deployment-global, no merchant scope.

-- name: AttachEventStore :one
-- Records the store's identity on first attach and answers the one recorded.
WITH recorded AS (
    INSERT INTO billing.event_store (singleton, store_id)
    VALUES (true, sqlc.arg(store_id)::uuid)
    ON CONFLICT (singleton) DO NOTHING
    RETURNING store_id
)
SELECT store_id FROM recorded
UNION ALL
SELECT e.store_id FROM billing.event_store e WHERE e.singleton AND NOT EXISTS (SELECT 1 FROM recorded);

-- name: ListUsageCatalogObjects :many
-- The meters, limits, rate cards and product limits each merchant keeps,
-- dormant on a deployment without an event store.
SELECT o.merchant_id, o.kind, count(*)::bigint AS objects
FROM (
    SELECT merchant_id, 'meters'::text AS kind FROM billing.catalog_meters
    UNION ALL SELECT merchant_id, 'limits'::text FROM billing.catalog_limits
    UNION ALL SELECT merchant_id, 'rate_cards'::text FROM billing.catalog_rate_cards
    UNION ALL SELECT merchant_id, 'product_limits'::text FROM billing.catalog_product_limits
) o
GROUP BY o.merchant_id, o.kind
ORDER BY o.merchant_id, o.kind;

-- name: LockEventStore :one
-- The attached store's identity, locked against another attach.
SELECT store_id FROM billing.event_store WHERE singleton FOR UPDATE;

-- name: ReattachEventStore :exec
-- An operator attaches another store: its identity replaces the recorded one.
UPDATE billing.event_store SET store_id = sqlc.arg(store_id)::uuid, attached_at = sqlc.arg(now)::timestamptz WHERE singleton;

-- Without an event store, applying a document that declares no usage removes
-- the merchant's dormant usage catalog, dependents first.

-- name: DeleteDormantRateCards :exec
DELETE FROM billing.catalog_rate_cards WHERE merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: DeleteDormantProductLimits :exec
DELETE FROM billing.catalog_product_limits WHERE merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: DeleteDormantLimitOverrides :exec
DELETE FROM billing.customer_limit_overrides WHERE merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: DeleteDormantLimitAdjustments :exec
DELETE FROM billing.limit_adjustments WHERE merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: DeleteDormantLimitWindows :exec
DELETE FROM billing.limit_windows WHERE merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: DeleteDormantLimits :exec
DELETE FROM billing.catalog_limits WHERE merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: DeleteDormantMeters :exec
DELETE FROM billing.catalog_meters WHERE merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: DeleteDormantUsageOwners :exec
DELETE FROM billing.catalog_field_owners
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (object IN ('meter', 'limit') OR (object = 'product' AND field IN ('rate_cards', 'limits')));

-- name: CountMerchantUsageCatalog :one
-- One merchant's dormant usage catalog.
SELECT (SELECT count(*) FROM billing.catalog_meters m WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid)::bigint AS meters,
       (SELECT count(*) FROM billing.catalog_limits l WHERE l.merchant_id = sqlc.arg(merchant_id)::uuid)::bigint AS limits;
