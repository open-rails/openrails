-- billing.product_access: the product windows customers hold, projected from
-- access grants. A window is live while it is neither revoked nor deleted;
-- deleted windows never granted access.

-- name: MaterializeProductAccess :exec
-- Concurrent replay of one immutable grant cannot duplicate its projection.
INSERT INTO billing.product_access (
    merchant_id, customer_id, product_id, grant_id, source_type, source_id, payment_id, starts_at, ends_at, quantity
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(product_id)::uuid, sqlc.arg(grant_id)::uuid,
    sqlc.arg(source_type)::text, sqlc.arg(source_id)::text, sqlc.narg(payment_id)::uuid,
    sqlc.arg(starts_at)::timestamptz, sqlc.narg(ends_at)::timestamptz, sqlc.narg(quantity)::int
)
ON CONFLICT (merchant_id, grant_id) WHERE deleted_at IS NULL DO NOTHING;

-- name: GetProductAccessByGrant :one
SELECT * FROM billing.product_access
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND grant_id = sqlc.arg(grant_id)::uuid AND deleted_at IS NULL;

-- name: ProductAccessExistsForGrant :one
SELECT EXISTS (SELECT 1 FROM billing.product_access
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND grant_id = sqlc.arg(grant_id)::uuid AND deleted_at IS NULL);

-- name: RevokeProductAccessByGrant :execrows
-- A terminated grant retracts its window: started windows are revoked, a
-- window that has not started is removed.
UPDATE billing.product_access pa SET
    revoked_at = CASE WHEN pa.starts_at <= sqlc.arg(revoked_at)::timestamptz THEN sqlc.arg(revoked_at)::timestamptz END,
    revoke_reason = CASE WHEN pa.starts_at <= sqlc.arg(revoked_at)::timestamptz THEN sqlc.arg(revoke_reason)::text END,
    deleted_at = CASE WHEN pa.starts_at > sqlc.arg(revoked_at)::timestamptz THEN sqlc.arg(revoked_at)::timestamptz END,
    updated_at = sqlc.arg(revoked_at)::timestamptz
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.grant_id = sqlc.arg(grant_id)::uuid
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL;

-- name: GetProductAccessByID :one
SELECT * FROM billing.product_access
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND deleted_at IS NULL;

-- name: GetLatestProductAccessBySource :one
SELECT * FROM billing.product_access
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND product_id = sqlc.arg(product_id)::uuid
  AND source_type = sqlc.arg(source_type)::text AND source_id = sqlc.arg(source_id)::text
  AND deleted_at IS NULL
ORDER BY (revoked_at IS NULL) DESC, ends_at DESC NULLS FIRST, starts_at ASC, id ASC
LIMIT 1;

-- name: ProductAccessExistsBySource :one
SELECT EXISTS (
    SELECT 1 FROM billing.product_access pa
    WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid
      AND pa.source_type = sqlc.arg(source_type)::text AND pa.source_id = sqlc.arg(source_id)::text
      AND pa.product_id = sqlc.arg(product_id)::uuid
      -- A purchase projects once: a revoked or retracted window is final.
      AND (pa.source_type = 'purchase' OR (pa.revoked_at IS NULL AND pa.deleted_at IS NULL))
);

-- name: ListLiveProductsBySource :many
-- The products a source (a subscription or grace allowance) gives now or later.
SELECT DISTINCT pa.product_id FROM billing.product_access pa
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid
  AND pa.source_type = sqlc.arg(source_type)::text AND pa.source_id = sqlc.arg(source_id)::text
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
ORDER BY pa.product_id;

-- name: AcquireAccessTimelineLock :exec
-- Transaction-scoped advisory lock serializing timeline updates per
-- (customer, product); the key is hashed in Go.
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);

-- name: GetAccessTimelineTailEnd :one
-- The latest finite end of a customer's live windows of a product: where a new
-- rental starts.
SELECT pa.ends_at FROM billing.product_access pa
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
  AND pa.product_id = sqlc.arg(product_id)::uuid
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND pa.ends_at IS NOT NULL
ORDER BY pa.ends_at DESC
LIMIT 1;

-- name: ProductAccessCoverage :one
-- Across the customer's live windows of a product at at: whether one is
-- indefinite, else the latest end.
SELECT COALESCE(bool_or(pa.ends_at IS NULL), false)::boolean AS indefinite,
       COALESCE(max(pa.ends_at), '0001-01-01 00:00:00+00'::timestamptz)::timestamptz AS latest_end_at
FROM billing.product_access pa
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
  AND pa.product_id = ANY(sqlc.arg(product_ids)::uuid[])
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at)::timestamptz);

-- name: EndActiveProductAccessBySubscription :exec
-- #691 closure write: bound a subscription's live windows to a PROVEN end.
-- Future-start windows are removed by SoftDeleteFutureProductAccessBySubscription.
UPDATE billing.product_access pa SET
    ends_at = sqlc.arg(ends_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.source_type = 'subscription'
  AND pa.source_id = sqlc.arg(source_id)::text
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND pa.starts_at < sqlc.arg(ends_at)::timestamptz
  AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(ends_at)::timestamptz);

-- name: SoftDeleteFutureProductAccessBySubscription :exec
UPDATE billing.product_access pa SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.source_type = 'subscription'
  AND pa.source_id = sqlc.arg(source_id)::text
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND pa.starts_at >= sqlc.arg(ends_at)::timestamptz;

-- name: RetractFutureProductAccessByPayment :exec
-- A refund removes the payment's windows that have not started; their grants
-- record the retraction.
WITH retracted AS (
UPDATE billing.product_access pa SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.payment_id = sqlc.arg(payment_id)::uuid
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND pa.starts_at >= sqlc.arg(ends_at)::timestamptz
    RETURNING pa.grant_id
)
INSERT INTO billing.grants (
    merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id,
    event, supersedes_id, starts_at, reason
)
SELECT g.merchant_id, g.customer_id, g.product_id, g.kind, g.source_type, g.source_id, g.payment_id,
       'revoke', g.id, sqlc.arg(now)::timestamptz, 'access window retracted'
FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.kind = 'access' AND g.event = 'grant'
  AND g.id IN (SELECT grant_id FROM retracted)
ORDER BY g.id
ON CONFLICT (merchant_id, supersedes_id)
WHERE supersedes_id IS NOT NULL AND event IN ('revoke', 'expire', 'supersede')
DO NOTHING;

-- name: RevokeActiveProductAccessByPayment :exec
-- A refund or chargeback ends the payment's started windows at ends_at and
-- revokes their grants.
WITH revoked AS (
UPDATE billing.product_access pa SET
    ends_at = sqlc.arg(ends_at)::timestamptz,
    revoked_at = sqlc.arg(now)::timestamptz,
    revoke_reason = sqlc.arg(revoke_reason)::text,
    updated_at = sqlc.arg(now)::timestamptz
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.payment_id = sqlc.arg(payment_id)::uuid
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND pa.starts_at < sqlc.arg(ends_at)::timestamptz
  AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(ends_at)::timestamptz)
    RETURNING pa.grant_id
)
INSERT INTO billing.grants (
    merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id,
    event, supersedes_id, starts_at, reason
)
SELECT g.merchant_id, g.customer_id, g.product_id, g.kind, g.source_type, g.source_id, g.payment_id,
       'revoke', g.id, sqlc.arg(now)::timestamptz, sqlc.arg(revoke_reason)::text
FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.kind = 'access' AND g.event = 'grant'
  AND g.id IN (SELECT grant_id FROM revoked)
ORDER BY g.id
ON CONFLICT (merchant_id, supersedes_id)
WHERE supersedes_id IS NOT NULL AND event IN ('revoke', 'expire', 'supersede')
DO NOTHING;

-- name: RevokeActiveProductTimeline :exec
-- Revoke a customer's started windows of a product, optionally of one source.
UPDATE billing.product_access pa SET
    revoked_at = sqlc.arg(now)::timestamptz,
    revoke_reason = sqlc.arg(revoke_reason)::text,
    updated_at = sqlc.arg(now)::timestamptz
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
  AND pa.product_id = sqlc.arg(product_id)::uuid
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND pa.starts_at <= sqlc.arg(now)::timestamptz
  AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(now)::timestamptz)
  AND (sqlc.narg(source_type)::text IS NULL OR pa.source_type = sqlc.narg(source_type)::text)
  AND (sqlc.narg(source_id)::text IS NULL OR pa.source_id = sqlc.narg(source_id)::text);

-- name: SoftDeleteFutureProductTimeline :exec
-- Remove a customer's future windows of a product, optionally of one source;
-- their grants record the retraction.
WITH retracted AS (
UPDATE billing.product_access pa SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
  AND pa.product_id = sqlc.arg(product_id)::uuid
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND pa.starts_at > sqlc.arg(now)::timestamptz
  AND (sqlc.narg(source_type)::text IS NULL OR pa.source_type = sqlc.narg(source_type)::text)
  AND (sqlc.narg(source_id)::text IS NULL OR pa.source_id = sqlc.narg(source_id)::text)
    RETURNING pa.grant_id
)
INSERT INTO billing.grants (
    merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id,
    event, supersedes_id, starts_at, reason
)
SELECT g.merchant_id, g.customer_id, g.product_id, g.kind, g.source_type, g.source_id, g.payment_id,
       'revoke', g.id, sqlc.arg(now)::timestamptz, 'access window retracted'
FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.kind = 'access' AND g.event = 'grant'
  AND g.id IN (SELECT grant_id FROM retracted)
ORDER BY g.id
ON CONFLICT (merchant_id, supersedes_id)
WHERE supersedes_id IS NOT NULL AND event IN ('revoke', 'expire', 'supersede')
DO NOTHING;

-- name: RevokeProductAccessByID :execrows
UPDATE billing.product_access pa SET
    revoked_at = sqlc.arg(now)::timestamptz,
    revoke_reason = sqlc.arg(revoke_reason)::text,
    updated_at = sqlc.arg(now)::timestamptz
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.id = sqlc.arg(id)::uuid
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL;

-- name: SoftDeleteProductAccessByID :exec
WITH retracted AS (
UPDATE billing.product_access pa SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.id = sqlc.arg(id)::uuid
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
    RETURNING pa.grant_id
)
INSERT INTO billing.grants (
    merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id,
    event, supersedes_id, starts_at, reason
)
SELECT g.merchant_id, g.customer_id, g.product_id, g.kind, g.source_type, g.source_id, g.payment_id,
       'revoke', g.id, sqlc.arg(now)::timestamptz, 'access window retracted'
FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.kind = 'access' AND g.event = 'grant'
  AND g.id IN (SELECT grant_id FROM retracted)
ON CONFLICT (merchant_id, supersedes_id)
WHERE supersedes_id IS NOT NULL AND event IN ('revoke', 'expire', 'supersede')
DO NOTHING;

-- name: HasPermanentProductAccess :one
-- Whether the customer holds the product indefinitely from at on.
SELECT EXISTS (
    SELECT 1 FROM billing.product_access pa
    WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
      AND pa.product_id = sqlc.arg(product_id)::uuid
      AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
      AND pa.ends_at IS NULL AND pa.starts_at <= sqlc.arg(at_time)::timestamptz
);

-- name: ListProductAccessPage :many
-- One keyset page of windows, newest first, of the named customers and
-- products (null: any): live at at_time when live_only, else every window
-- that was not removed.
SELECT pa.*, p.key AS product_key, p.display_name AS product_name, g.grant_reason, g.actor, g.reason AS note
FROM billing.product_access pa
JOIN billing.products p ON p.merchant_id = pa.merchant_id AND p.id = pa.product_id
JOIN billing.grants g ON g.merchant_id = pa.merchant_id AND g.customer_id = pa.customer_id AND g.id = pa.grant_id
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_ids)::uuid[] IS NULL OR pa.customer_id = ANY (sqlc.narg(customer_ids)::uuid[]))
  AND (sqlc.narg(product_ids)::uuid[] IS NULL OR pa.product_id = ANY (sqlc.narg(product_ids)::uuid[]))
  AND pa.deleted_at IS NULL
  AND (NOT sqlc.arg(live_only)::boolean OR (pa.revoked_at IS NULL
       AND pa.starts_at <= sqlc.arg(at_time)::timestamptz
       AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at_time)::timestamptz)))
  AND (sqlc.narg(after_id)::uuid IS NULL OR pa.id < sqlc.narg(after_id)::uuid)
ORDER BY pa.id DESC
LIMIT sqlc.arg(fetch_limit)::int;

-- name: ListProductAccessViews :many
SELECT pa.*, p.key AS product_key, p.display_name AS product_name, g.grant_reason, g.actor, g.reason AS note
FROM billing.product_access pa
JOIN billing.products p ON p.merchant_id = pa.merchant_id AND p.id = pa.product_id
JOIN billing.grants g ON g.merchant_id = pa.merchant_id AND g.customer_id = pa.customer_id AND g.id = pa.grant_id
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.id = ANY(sqlc.arg(ids)::uuid[]) AND pa.deleted_at IS NULL;

-- name: GetLatestLiveProductEnd :one
-- The latest end of the customer's live windows of a product (NULL:
-- indefinite), for a grant that extends after them.
SELECT pa.ends_at FROM billing.product_access pa
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
  AND pa.product_id = sqlc.arg(product_id)::uuid
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at)::timestamptz)
ORDER BY pa.ends_at DESC NULLS FIRST
LIMIT 1;

-- name: CountLiveProductHolders :many
-- How many customers hold each product at at.
SELECT candidate.product_id::uuid AS product_id, (
    SELECT count(DISTINCT pa.customer_id) FROM billing.product_access pa
    WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.product_id = candidate.product_id
      AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
      AND pa.starts_at <= sqlc.arg(at)::timestamptz
      AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at)::timestamptz)
)::bigint AS holders
FROM unnest(sqlc.arg(product_ids)::uuid[]) AS candidate(product_id);

-- name: ListLiveAccessBySubscriptions :many
-- The live windows the listed subscriptions give at at_time.
SELECT * FROM billing.product_access pa
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid
  AND pa.source_type IN ('subscription', 'grace') AND pa.source_id = ANY(sqlc.arg(source_ids)::text[])
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND pa.starts_at <= sqlc.arg(at_time)::timestamptz
  AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at_time)::timestamptz)
ORDER BY pa.source_id, pa.ends_at DESC NULLS FIRST, pa.id;
