-- Derived entitlements: a customer holds a key at an instant when a live
-- product_access window of theirs covers it and the product granted the key
-- then (product_entitlements valid time). Nothing per customer stores keys.

-- name: CheckDerivedEntitlements :many
-- Key-first: for each key, the products granting it, then the customer's
-- live windows of each, for the most seats one gives. Cost: keys x products
-- granting each key; the LATERAL keeps the planner from scanning every window
-- the customer holds instead.
SELECT k.key::text AS entitlement, (held.windows > 0)::boolean AS has_access,
       COALESCE(held.quantity, 0)::int AS quantity
FROM unnest(sqlc.arg(entitlements)::text[]) AS k(key)
CROSS JOIN LATERAL (
    SELECT count(*) AS windows, max(pa.quantity) AS quantity FROM billing.product_entitlements pe
    JOIN billing.product_access pa ON pa.merchant_id = pe.merchant_id AND pa.product_id = pe.product_id
    WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid AND pe.entitlement = k.key
      AND pe.added_at <= sqlc.arg(at_time)::timestamptz
      AND (pe.removed_at IS NULL OR pe.removed_at > sqlc.arg(at_time)::timestamptz)
      AND pa.customer_id = sqlc.arg(customer_id)::uuid
      AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
      AND pa.starts_at <= sqlc.arg(at_time)::timestamptz
      AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at_time)::timestamptz)
) held;

-- name: ListDerivedEntitlementsByPrefix :many
-- Customer-first, one pass: the products the customer holds, then one probe
-- of each product's keys within [low, high), then the first row_limit keys
-- of each prefix in byte order (callers pass limit+1 to detect truncation).
-- The OFFSET 0 fence keeps the planner from scanning the catalog keyspace.
-- The key range is bounded as (product, key) rows: only the per-product index
-- can use it, so no plan scans a key range of the catalog once per held
-- product. Callers plan it for their parameters (a generic plan prices a
-- 3-product and a 50,000-product customer alike).
WITH owned AS MATERIALIZED (
    SELECT DISTINCT pa.product_id FROM billing.product_access pa
    WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
      AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
      AND pa.starts_at <= sqlc.arg(at_time)::timestamptz
      AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at_time)::timestamptz)
), keys AS MATERIALIZED (
    SELECT DISTINCT k.entitlement FROM owned o
    CROSS JOIN LATERAL (
        SELECT pe.entitlement FROM billing.product_entitlements pe
        WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid AND pe.product_id = o.product_id
          AND (pe.product_id, pe.entitlement) >= (o.product_id, sqlc.arg(low)::text)
          AND (pe.product_id, pe.entitlement) < (o.product_id, sqlc.arg(high)::text)
          AND pe.added_at <= sqlc.arg(at_time)::timestamptz
          AND (pe.removed_at IS NULL OR pe.removed_at > sqlc.arg(at_time)::timestamptz)
        OFFSET 0
    ) k
)
SELECT r.prefix::text AS prefix, h.entitlement::text AS entitlement
FROM unnest(sqlc.arg(prefixes)::text[], sqlc.arg(uppers)::text[]) AS r(prefix, upper)
CROSS JOIN LATERAL (
    SELECT keys.entitlement FROM keys
    WHERE keys.entitlement >= r.prefix COLLATE "C" AND keys.entitlement < r.upper COLLATE "C"
    ORDER BY keys.entitlement COLLATE "C"
    LIMIT sqlc.arg(row_limit)::int
) h
ORDER BY r.prefix COLLATE "C", h.entitlement COLLATE "C";

-- name: ListDerivedEntitlementsPage :many
-- One keyset page of the keys a customer holds at at_time, in byte order:
-- from low_key, after after_key (exclusive; '' starts) and below before_key
-- (exclusive; '' is unbounded). Customer-first, one probe per held product;
-- the bounds are (product, key) rows, as in ListDerivedEntitlementsByPrefix.
WITH owned AS MATERIALIZED (
    SELECT DISTINCT pa.product_id FROM billing.product_access pa
    WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.customer_id = sqlc.arg(customer_id)::uuid
      AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
      AND pa.starts_at <= sqlc.arg(at_time)::timestamptz
      AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at_time)::timestamptz)
)
SELECT DISTINCT k.entitlement FROM owned o
CROSS JOIN LATERAL (
    SELECT pe.entitlement FROM billing.product_entitlements pe
    WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid AND pe.product_id = o.product_id
      AND (pe.product_id, pe.entitlement) >= (o.product_id, sqlc.arg(low_key)::text)
      AND (pe.product_id, pe.entitlement) > (o.product_id, sqlc.arg(after_key)::text)
      AND (sqlc.arg(before_key)::text = '' OR pe.entitlement < sqlc.arg(before_key)::text)
      AND pe.added_at <= sqlc.arg(at_time)::timestamptz
      AND (pe.removed_at IS NULL OR pe.removed_at > sqlc.arg(at_time)::timestamptz)
    ORDER BY pe.entitlement COLLATE "C"
    LIMIT sqlc.arg(row_limit)::int
) k
ORDER BY 1
LIMIT sqlc.arg(row_limit)::int;

-- name: ListDerivedEntitlementHolders :many
-- The reverse lookup: customers holding a key at at_time, keyset by customer
-- id after after_id. Key-first: the products granting it, then each
-- product's holders in customer order, stopping at the limit per product.
-- The cursor bounds (product, customer) rows, so only the per-product index
-- serves the probe.
SELECT DISTINCT x.customer_id FROM billing.product_entitlements pe
CROSS JOIN LATERAL (
    SELECT pa.customer_id FROM billing.product_access pa
    WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.product_id = pe.product_id
      AND (pa.product_id, pa.customer_id) > (pe.product_id, sqlc.arg(after_id)::uuid)
      AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
      AND pa.starts_at <= sqlc.arg(at_time)::timestamptz
      AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at_time)::timestamptz)
    ORDER BY pa.customer_id
    LIMIT sqlc.arg(row_limit)::int
) x
WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid AND pe.entitlement = sqlc.arg(entitlement)::text
  AND pe.added_at <= sqlc.arg(at_time)::timestamptz
  AND (pe.removed_at IS NULL OR pe.removed_at > sqlc.arg(at_time)::timestamptz)
ORDER BY 1
LIMIT sqlc.arg(row_limit)::int;

-- name: ListEntitlementSources :many
-- Why a customer holds each key at at_time: every live window of a product
-- granting it, with the window's source and the key's catalog provenance.
SELECT k.key::text AS entitlement, pa.id AS access_id, pa.product_id, p.key AS product_key,
       pa.source_type, pa.source_id, pa.payment_id, pa.starts_at, pa.ends_at,
       g.grant_reason, g.actor, pe.added_at AS key_added_at, pe.added_by AS key_added_by
FROM unnest(sqlc.arg(entitlements)::text[]) AS k(key)
JOIN billing.product_entitlements pe ON pe.merchant_id = sqlc.arg(merchant_id)::uuid AND pe.entitlement = k.key
JOIN billing.product_access pa ON pa.merchant_id = pe.merchant_id AND pa.product_id = pe.product_id
JOIN billing.products p ON p.merchant_id = pa.merchant_id AND p.id = pa.product_id
JOIN billing.grants g ON g.merchant_id = pa.merchant_id AND g.customer_id = pa.customer_id AND g.id = pa.grant_id
WHERE pe.added_at <= sqlc.arg(at_time)::timestamptz
  AND (pe.removed_at IS NULL OR pe.removed_at > sqlc.arg(at_time)::timestamptz)
  AND pa.customer_id = sqlc.arg(customer_id)::uuid
  AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
  AND pa.starts_at <= sqlc.arg(at_time)::timestamptz
  AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at_time)::timestamptz)
ORDER BY k.key COLLATE "C", pa.starts_at, pa.id;

-- name: ResolveEffectiveTiers :many
-- or#912: each customer's tier in a group: the highest-ranked product of the
-- group granting a key the customer holds at at (archived products included:
-- holders keep deriving them). Ties: product key, then key. A customer with
-- no tier has no row.
SELECT c.customer_id::uuid AS customer_id, t.product_id, t.product_key, t.product_display_name, t.tier_rank, t.entitlement
FROM unnest(sqlc.arg(customer_ids)::uuid[]) AS c(customer_id)
CROSS JOIN LATERAL (
  SELECT p.id AS product_id, p.key AS product_key, p.display_name AS product_display_name, p.tier_rank,
         tier_key.entitlement::text AS entitlement
  FROM billing.products p
  JOIN billing.product_entitlements tier_key ON tier_key.merchant_id = p.merchant_id AND tier_key.product_id = p.id
  WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.tier_group = sqlc.arg(tier_group)::text
    AND tier_key.added_at <= sqlc.arg(at)::timestamptz
    AND (tier_key.removed_at IS NULL OR tier_key.removed_at > sqlc.arg(at)::timestamptz)
    AND EXISTS (
      SELECT 1 FROM billing.product_entitlements pe
      JOIN billing.product_access pa ON pa.merchant_id = pe.merchant_id AND pa.product_id = pe.product_id
      WHERE pe.merchant_id = p.merchant_id AND pe.entitlement = tier_key.entitlement
        AND pe.added_at <= sqlc.arg(at)::timestamptz
        AND (pe.removed_at IS NULL OR pe.removed_at > sqlc.arg(at)::timestamptz)
        AND pa.customer_id = c.customer_id
        AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
        AND pa.starts_at <= sqlc.arg(at)::timestamptz
        AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(at)::timestamptz))
  ORDER BY p.tier_rank DESC, p.key ASC, tier_key.entitlement COLLATE "C" ASC
  LIMIT 1
) t;

-- name: PermanentBenefitsCovered :one
-- A partial bundle remains useful; refuse only when every key of the product
-- is already held indefinitely or reserved by another accepted permanent
-- purchase. Admission calls this under the customer lock used by session and
-- sale insertion. Reservations name their product, whose current keys count.
WITH wanted AS (
    SELECT pe.entitlement FROM billing.product_entitlements pe
    WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid AND pe.product_id = sqlc.arg(product_id)::uuid AND pe.removed_at IS NULL
), reserved AS (
    SELECT (s.rail_state->'accepted_purchase'->>'product_id')::uuid AS product_id
    FROM billing.checkout_attempts s
    WHERE sqlc.arg(include_pending)::boolean
      AND s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.customer_id = sqlc.arg(customer_id)::uuid
      AND s.id <> sqlc.arg(except_session_id)::uuid AND s.mode = 'one_off' AND s.status <> 'succeeded'
      AND (s.status IN ('created','requires_action') OR (s.rail IN ('stripe','solana') AND NOT COALESCE((s.rail_state->>'provider_closed')::boolean,false)))
      AND s.rail_state->'accepted_purchase'->>'access_duration_hours' IS NULL
      AND s.rail_state->'accepted_purchase'->>'product_id' IS NOT NULL
      -- #1099: a session whose sale finally failed reserves nothing; its
      -- operation's outcome is the session's.
      AND NOT EXISTS (SELECT 1 FROM billing.provider_intents f
        WHERE f.merchant_id=s.merchant_id
          AND f.idempotency_key IN ('nmi_sale:checkout_native_session:'||s.id::text, 'custodian_sale:checkout_native_session:'||s.id::text)
          AND f.status IN ('failed_terminal','expired','superseded'))
    UNION
    SELECT (i.payload->>'product_id')::uuid
    FROM billing.provider_intents i
    WHERE sqlc.arg(include_pending)::boolean
      AND i.merchant_id = sqlc.arg(merchant_id)::uuid AND i.intent_type = 'nmi_sale'
      AND i.payload->>'user_id' = sqlc.arg(customer_id)::uuid::text
      AND i.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')
      AND i.payload->>'access_duration_hours' IS NULL
      AND i.payload->>'product_id' IS NOT NULL
)
SELECT EXISTS (SELECT 1 FROM wanted) AND NOT EXISTS (
    SELECT 1 FROM wanted w
    WHERE NOT EXISTS (
        SELECT 1 FROM billing.product_entitlements pe
        JOIN billing.product_access pa ON pa.merchant_id = pe.merchant_id AND pa.product_id = pe.product_id
        WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid AND pe.entitlement = w.entitlement AND pe.removed_at IS NULL
          AND pa.customer_id = sqlc.arg(customer_id)::uuid AND pa.ends_at IS NULL
          AND pa.starts_at <= sqlc.arg(at_time)::timestamptz
          AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
    ) AND NOT EXISTS (
        SELECT 1 FROM reserved r
        JOIN billing.product_entitlements pe ON pe.merchant_id = sqlc.arg(merchant_id)::uuid AND pe.product_id = r.product_id
        WHERE pe.entitlement = w.entitlement AND pe.removed_at IS NULL
    )
) AS covered;
