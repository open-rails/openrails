-- A heavy buyer's cached keys. Every read checks the stamps in its own
-- snapshot: an entry answers only while the merchant's entitlement
-- generation and the customer's access version are the ones it was built at,
-- for instants in its window. Otherwise the reader derives live.

-- name: EntitlementCacheValid :one
SELECT EXISTS (
    SELECT 1 FROM billing.customer_entitlement_cache_stamps s
    JOIN billing.merchants m ON m.id = s.merchant_id
    JOIN billing.customers c ON c.merchant_id = s.merchant_id AND c.id = s.customer_id
    WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.customer_id = sqlc.arg(customer_id)::uuid
      AND s.entitlement_generation = m.entitlement_generation AND s.access_version = c.access_version
      AND s.valid_from <= sqlc.arg(at_time)::timestamptz
      AND (s.valid_until IS NULL OR s.valid_until > sqlc.arg(at_time)::timestamptz)
)::boolean AS valid;

-- name: CheckCachedEntitlements :many
-- One probe of the cache index per key.
SELECT k.key::text AS entitlement, (held.found IS NOT NULL)::boolean AS has_access,
       COALESCE(held.quantity, 0)::int AS quantity
FROM unnest(sqlc.arg(entitlements)::text[]) AS k(key)
LEFT JOIN LATERAL (
    SELECT true AS found, ec.quantity FROM billing.customer_entitlement_cache ec
    WHERE ec.merchant_id = sqlc.arg(merchant_id)::uuid AND ec.customer_id = sqlc.arg(customer_id)::uuid
      AND ec.entitlement = k.key
    LIMIT 1
) held ON true;

-- name: ListCachedEntitlementsByPrefix :many
-- The first row_limit keys under each prefix, one range of the cache index each.
SELECT r.prefix::text AS prefix, h.entitlement::text AS entitlement
FROM unnest(sqlc.arg(prefixes)::text[], sqlc.arg(uppers)::text[]) AS r(prefix, upper)
CROSS JOIN LATERAL (
    SELECT ec.entitlement FROM billing.customer_entitlement_cache ec
    WHERE ec.merchant_id = sqlc.arg(merchant_id)::uuid AND ec.customer_id = sqlc.arg(customer_id)::uuid
      AND ec.entitlement >= r.prefix AND ec.entitlement < r.upper
    ORDER BY ec.entitlement
    LIMIT sqlc.arg(row_limit)::int
) h
ORDER BY r.prefix COLLATE "C", h.entitlement COLLATE "C";

-- name: ListCachedEntitlementsPage :many
SELECT ec.entitlement::text AS entitlement FROM billing.customer_entitlement_cache ec
WHERE ec.merchant_id = sqlc.arg(merchant_id)::uuid AND ec.customer_id = sqlc.arg(customer_id)::uuid
  AND ec.entitlement >= sqlc.arg(low_key)::text AND ec.entitlement > sqlc.arg(after_key)::text
  AND (sqlc.arg(before_key)::text = '' OR ec.entitlement < sqlc.arg(before_key)::text)
ORDER BY ec.entitlement
LIMIT sqlc.arg(row_limit)::int;

-- name: CountHeldProductsUpTo :one
-- How many products the customer holds at at_time, counting at most up_to.
SELECT count(*)::int AS held FROM (
    SELECT DISTINCT pa.product_id FROM billing.product_access pa
    WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
      AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
      AND pa.starts_at <= sqlc.arg(at_time)::timestamptz
      AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at_time)::timestamptz)
    LIMIT sqlc.arg(up_to)::int
) held;

-- name: TryLockEntitlementCache :one
-- One rebuild per customer at a time; another waits for nothing.
SELECT pg_try_advisory_xact_lock(hashtextextended('openrails.entitlement_cache:' || sqlc.arg(customer_id)::uuid::text, 0))::boolean AS locked;

-- name: GetAccessStamps :one
SELECT m.entitlement_generation, c.access_version FROM billing.merchants m
JOIN billing.customers c ON c.merchant_id = m.id
WHERE m.id = sqlc.arg(merchant_id)::uuid AND c.id = sqlc.arg(customer_id)::uuid;

-- name: GetNextAccessBoundary :one
-- The next instant after at_time at which one of the customer's windows
-- starts or ends: their keys hold until then. No row: never.
SELECT b.at::timestamptz AS next_at FROM billing.product_access pa
CROSS JOIN LATERAL (VALUES (pa.starts_at), (pa.ends_at)) AS b(at)
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at_time)::timestamptz)
  AND b.at > sqlc.arg(at_time)::timestamptz
ORDER BY b.at
LIMIT 1;

-- name: DeleteEntitlementCache :exec
DELETE FROM billing.customer_entitlement_cache
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid;

-- name: SyncEntitlementCache :one
-- Makes the cache the keys the customer holds at at_time and their seats,
-- writing only the difference from what it held: one probe of each held
-- product, one full join of held and cached keys (a hash or merge join, never
-- a nested loop, whatever the cache's statistics say) and one index probe per
-- stale or changed key.
WITH owned AS MATERIALIZED (
    SELECT pa.product_id, max(pa.quantity) AS quantity FROM billing.product_access pa
    WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
      AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
      AND pa.starts_at <= sqlc.arg(at_time)::timestamptz
      AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at_time)::timestamptz)
    GROUP BY pa.product_id
), held AS MATERIALIZED (
    SELECT k.entitlement, max(o.quantity) AS quantity FROM owned o
    CROSS JOIN LATERAL (
        SELECT pe.entitlement FROM billing.product_entitlements pe
        WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid AND pe.product_id = o.product_id
          AND pe.added_at <= sqlc.arg(at_time)::timestamptz
          AND (pe.removed_at IS NULL OR pe.removed_at > sqlc.arg(at_time)::timestamptz)
        OFFSET 0
    ) k
    GROUP BY k.entitlement
), cached AS MATERIALIZED (
    SELECT ec.entitlement, ec.quantity FROM billing.customer_entitlement_cache ec
    WHERE ec.merchant_id = sqlc.arg(merchant_id)::uuid AND ec.customer_id = sqlc.arg(customer_id)::uuid
), diff AS MATERIALIZED (
    SELECT coalesce(held.entitlement, cached.entitlement) AS entitlement, held.quantity, held.entitlement IS NULL AS stale
    FROM held FULL JOIN cached ON cached.entitlement = held.entitlement
    WHERE held.entitlement IS NULL OR cached.entitlement IS NULL OR cached.quantity IS DISTINCT FROM held.quantity
), dropped AS (
    DELETE FROM billing.customer_entitlement_cache ec
    WHERE ec.merchant_id = sqlc.arg(merchant_id)::uuid AND ec.customer_id = sqlc.arg(customer_id)::uuid
      AND ec.entitlement = ANY (ARRAY(SELECT diff.entitlement FROM diff WHERE diff.stale))
    RETURNING 1
), added AS (
    INSERT INTO billing.customer_entitlement_cache (merchant_id, customer_id, entitlement, quantity)
    SELECT sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, diff.entitlement, diff.quantity FROM diff WHERE NOT diff.stale
    ON CONFLICT (merchant_id, customer_id, entitlement) DO UPDATE SET quantity = EXCLUDED.quantity
    RETURNING 1
)
SELECT (SELECT count(*) FROM held)::int AS keys, (SELECT count(*) FROM dropped)::int AS dropped, (SELECT count(*) FROM added)::int AS added;

-- name: UpsertEntitlementCacheStamps :exec
INSERT INTO billing.customer_entitlement_cache_stamps
    (merchant_id, customer_id, entitlement_generation, access_version, valid_from, valid_until, keys, held_products)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(entitlement_generation)::bigint, sqlc.arg(access_version)::bigint,
        sqlc.arg(valid_from)::timestamptz, sqlc.narg(valid_until)::timestamptz, sqlc.arg(keys)::int, sqlc.arg(held_products)::int)
ON CONFLICT (merchant_id, customer_id) DO UPDATE SET
    entitlement_generation = EXCLUDED.entitlement_generation, access_version = EXCLUDED.access_version,
    valid_from = EXCLUDED.valid_from, valid_until = EXCLUDED.valid_until,
    keys = EXCLUDED.keys, held_products = EXCLUDED.held_products, updated_at = now();

-- name: DropEntitlementCacheStamps :exec
-- A customer who no longer holds enough products to cache.
DELETE FROM billing.customer_entitlement_cache_stamps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid;
