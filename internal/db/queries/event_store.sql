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
