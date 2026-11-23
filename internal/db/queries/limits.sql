-- name: ListCatalogLimits :many
SELECT key, unit, reset_kind, window_seconds, reset_anchor, overrun, rate_cards, revision, created_at, updated_at
FROM billing.catalog_limits
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY key;

-- name: PageCatalogLimits :many
SELECT key, unit, reset_kind, window_seconds, reset_anchor, overrun, rate_cards, revision, created_at, updated_at
FROM billing.catalog_limits
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(after_key)::text IS NULL OR key > sqlc.narg(after_key)::text)
ORDER BY key
LIMIT sqlc.arg(fetch_limit)::int;

-- name: GetCatalogLimit :one
SELECT key, unit, reset_kind, window_seconds, reset_anchor, overrun, rate_cards, revision, created_at, updated_at
FROM billing.catalog_limits
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND key = sqlc.arg(limit_key)::text;

-- name: UpsertCatalogLimit :exec
INSERT INTO billing.catalog_limits (merchant_id, key, unit, reset_kind, window_seconds, reset_anchor, overrun, rate_cards)
VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(limit_key)::text, sqlc.arg(unit)::text, sqlc.arg(reset_kind)::text,
    sqlc.narg(window_seconds)::int, sqlc.narg(reset_anchor)::text, sqlc.arg(overrun)::text, sqlc.arg(rate_cards)::jsonb
)
ON CONFLICT (merchant_id, key) DO UPDATE SET unit = EXCLUDED.unit, reset_kind = EXCLUDED.reset_kind,
    window_seconds = EXCLUDED.window_seconds, reset_anchor = EXCLUDED.reset_anchor, overrun = EXCLUDED.overrun,
    rate_cards = EXCLUDED.rate_cards, updated_at = now();

-- name: CatalogLimitHasWindows :one
SELECT EXISTS (
    SELECT 1 FROM billing.limit_windows
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND limit_key = sqlc.arg(limit_key)::text
);

-- name: ListCatalogLimitsUsingMeters :many
SELECT DISTINCT l.key
FROM billing.catalog_limits l, jsonb_array_elements(l.rate_cards) card
WHERE l.merchant_id = sqlc.arg(merchant_id)::uuid
  AND card->>'meter' = ANY(sqlc.arg(meter_keys)::text[])
ORDER BY l.key;

-- name: ListCatalogProductLimits :many
SELECT pl.product_id, p.key AS product_key, pl.limit_key, pl.capacity
FROM billing.catalog_product_limits pl
JOIN billing.products p ON p.merchant_id = pl.merchant_id AND p.id = pl.product_id
WHERE pl.merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY p.key, pl.limit_key;

-- name: ListCatalogLimitProducts :many
SELECT pl.product_id, p.key AS product_key, pl.limit_key, pl.capacity
FROM billing.catalog_product_limits pl
JOIN billing.products p ON p.merchant_id = pl.merchant_id AND p.id = pl.product_id
WHERE pl.merchant_id = sqlc.arg(merchant_id)::uuid AND pl.limit_key = ANY(sqlc.arg(limit_keys)::text[])
ORDER BY pl.limit_key, p.key;

-- name: UpsertCatalogProductLimit :exec
INSERT INTO billing.catalog_product_limits (merchant_id, product_id, limit_key, capacity)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(product_id)::uuid, sqlc.arg(limit_key)::text, sqlc.arg(capacity)::bigint)
ON CONFLICT (merchant_id, product_id, limit_key) DO UPDATE SET capacity = EXCLUDED.capacity, updated_at = now();

-- name: DeleteCatalogProductLimit :exec
DELETE FROM billing.catalog_product_limits
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid
  AND limit_key = sqlc.arg(limit_key)::text;

-- A customer's capacity per limit: the sum over its distinct live products.
-- name: ListCustomerProductLimitCapacities :many
SELECT pl.limit_key, SUM(pl.capacity)::bigint AS capacity
FROM billing.catalog_product_limits pl
WHERE pl.merchant_id = sqlc.arg(merchant_id)::uuid
  AND pl.product_id IN (
      SELECT pa.product_id FROM billing.product_access pa
      WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
        AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
        AND pa.starts_at <= sqlc.arg(at)::timestamptz
        AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at)::timestamptz))
GROUP BY pl.limit_key
ORDER BY pl.limit_key;

-- name: ListCustomerLimitOverrides :many
SELECT customer_id, limit_key, capacity
FROM billing.customer_limit_overrides
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
ORDER BY customer_id, limit_key;

-- name: UpsertCustomerLimitOverride :exec
INSERT INTO billing.customer_limit_overrides (merchant_id, customer_id, limit_key, capacity)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(limit_key)::text, sqlc.arg(capacity)::bigint)
ON CONFLICT (merchant_id, customer_id, limit_key) DO UPDATE SET capacity = EXCLUDED.capacity, updated_at = now();

-- name: DeleteCustomerLimitOverride :exec
DELETE FROM billing.customer_limit_overrides
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND limit_key = sqlc.arg(limit_key)::text;

-- The start of the customer's earliest live subscription to a product that
-- gives the limit capacity: what a subscription-anchored window tiles from.
-- name: GetLimitSubscriptionAnchor :one
SELECT s.started_at
FROM billing.subscriptions s
JOIN billing.catalog_product_limits pl
  ON pl.merchant_id = s.merchant_id AND pl.product_id = s.product_id AND pl.limit_key = sqlc.arg(limit_key)::text
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.customer_id = sqlc.arg(customer_id)::uuid
  AND s.ended_at IS NULL AND s.deleted_at IS NULL AND s.status <> 'pending'
ORDER BY s.started_at
LIMIT 1;

-- Each limit's newest window of the customer.
-- name: ListLatestLimitWindows :many
SELECT DISTINCT ON (limit_key) limit_key, window_starts_at, window_ends_at, settled, adjusted, updated_at
FROM billing.limit_windows
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
ORDER BY limit_key, window_starts_at DESC;

-- name: GetLimitWindow :one
SELECT limit_key, window_starts_at, window_ends_at, settled, adjusted, updated_at
FROM billing.limit_windows
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND limit_key = sqlc.arg(limit_key)::text AND window_starts_at = sqlc.arg(window_starts_at)::timestamptz;

-- A rolling limit's buckets from a start.
-- name: ListLimitWindowsFrom :many
SELECT limit_key, window_starts_at, window_ends_at, settled, adjusted, updated_at
FROM billing.limit_windows
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND limit_key = sqlc.arg(limit_key)::text AND window_starts_at >= sqlc.arg(window_starts_at)::timestamptz
ORDER BY window_starts_at;

-- An adjustment adds to one window's capacity; the window opens if it has
-- no row yet.
-- name: AdjustLimitWindow :exec
INSERT INTO billing.limit_windows AS w (merchant_id, customer_id, limit_key, window_starts_at, window_ends_at, capacity, settled, adjusted)
VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(limit_key)::text,
    sqlc.arg(window_starts_at)::timestamptz, sqlc.narg(window_ends_at)::timestamptz, sqlc.arg(capacity)::bigint, 0, sqlc.arg(amount)::bigint
)
ON CONFLICT (merchant_id, customer_id, limit_key, window_starts_at) DO UPDATE SET
    adjusted = w.adjusted + EXCLUDED.adjusted, capacity = EXCLUDED.capacity, updated_at = now();
