-- Audit consistency checks (internal/audit). One named query per check
-- query; SQL carried over essentially verbatim from the bun-era checks.
-- Options.UserID post-filtering stays in Go (matching the old behavior);
-- only S-E-1 / P-E-1 filter by customer_id in SQL because the bun
-- builder queries did.

-- ============================================================================
-- Subscription-entitlement checks
-- ============================================================================

-- S-E-1 (part 1): active subscriptions joined with price+product so the Go
-- side can walk product.entitlements_spec (bun loaded Price/Price.Product
-- relations; INNER JOIN matches the old "skip when relation is nil" logic).
-- name: AuditActiveSubscriptionsWithSpec :many
SELECT
    sub.id,
    sub.customer_id,
    prod.id AS product_id,
    prod.slug AS product_slug,
    prod.entitlements_spec
FROM openrails.subscriptions sub
JOIN openrails.prices price ON sub.price_id = price.id
JOIN openrails.products prod ON price.product_id = prod.id
WHERE sub.status = 'active'
  AND (sqlc.narg(customer_id)::uuid IS NULL OR sub.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(since)::timestamptz IS NULL OR sub.created_at >= sqlc.narg(since)::timestamptz);

-- S-E-1 (part 2): count currently-active entitlements granted by a
-- subscription (deleted_at filter was implicit via bun soft delete).
-- name: AuditActiveSubscriptionEntitlementCount :one
SELECT count(*) FROM openrails.entitlements ent
WHERE ent.customer_id = sqlc.arg(customer_id)
  AND ent.entitlement = sqlc.arg(entitlement)
  AND ent.source_type = 'subscription'
  AND ent.source_id = sqlc.arg(source_id)
  AND ent.revoked_at IS NULL
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(now)::timestamptz)
  AND ent.deleted_at IS NULL;

-- S-E-2
-- name: AuditOrphanSubscriptionEntitlements :many
SELECT
    ent.id AS entitlement_id,
    ent.customer_id::text AS user_id,
    ent.entitlement,
    ent.source_id,
    CASE WHEN sub.id IS NOT NULL THEN sub.status::text END AS sub_status
FROM openrails.entitlements ent
LEFT JOIN openrails.subscriptions sub ON ent.source_id = sub.id
WHERE ent.source_type = 'subscription'
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND (ent.end_at IS NULL OR ent.end_at > NOW())
  AND (sub.id IS NULL OR sub.status NOT IN ('active', 'pending', 'past_due'));

-- S-E-3
-- name: AuditCancelledSubscriptionActiveEntitlements :many
SELECT
    sub.id AS sub_id,
    sub.customer_id::text AS user_id,
    sub.ended_at::timestamptz AS ended_at,
    ent.id AS ent_id,
    ent.entitlement
FROM openrails.subscriptions sub
INNER JOIN openrails.entitlements ent ON ent.source_id = sub.id
WHERE sub.status = 'cancelled'
  AND sub.ended_at IS NOT NULL
  AND ent.source_type = 'subscription'
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND (ent.end_at IS NULL OR ent.end_at > NOW());

-- S-E-4
-- name: AuditWrongEntitlementEndDate :many
SELECT
    sub.id AS sub_id,
    sub.customer_id::text AS user_id,
    sub.current_period_ends_at::timestamptz AS period_ends_at,
    ent.id AS ent_id,
    ent.entitlement,
    ent.end_at AS ent_end_at
FROM openrails.subscriptions sub
INNER JOIN openrails.entitlements ent ON ent.source_id = sub.id
WHERE sub.status = 'cancelled'
  AND sub.cancelled_at IS NOT NULL
  AND sub.ended_at IS NULL
  AND sub.current_period_ends_at IS NOT NULL
  AND ent.source_type = 'subscription'
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND (ent.end_at IS NULL OR ent.end_at != sub.current_period_ends_at);

-- S-E-5
-- name: AuditEntitlementSourceMismatch :many
SELECT
    ent.id AS ent_id,
    ent.customer_id::text AS ent_user_id,
    ent.entitlement,
    ent.source_id,
    CASE WHEN sub.id IS NOT NULL THEN sub.customer_id::text END AS sub_user_id
FROM openrails.entitlements ent
LEFT JOIN openrails.subscriptions sub ON ent.source_id = sub.id
WHERE ent.source_type = 'subscription'
  AND ent.source_id IS NOT NULL
  AND ent.deleted_at IS NULL
  AND (sub.id IS NULL OR sub.customer_id != ent.customer_id);

-- ============================================================================
-- Payment-entitlement checks
-- ============================================================================

-- P-E-1 (part 1): one-off payments joined with price+product.
-- name: AuditOneOffPaymentsWithSpec :many
SELECT
    purch.id,
    purch.customer_id,
    purch.amount,
    purch.purchased_at,
    prod.id AS product_id,
    prod.slug AS product_slug,
    prod.entitlements_spec
FROM openrails.payments purch
JOIN openrails.prices price ON purch.price_id = price.id
JOIN openrails.products prod ON price.product_id = prod.id
WHERE purch.subscription_id IS NULL
  AND purch.amount > 0
  AND (sqlc.narg(customer_id)::uuid IS NULL OR purch.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(since)::timestamptz IS NULL OR purch.created_at >= sqlc.narg(since)::timestamptz);

-- P-E-1 (part 2): count entitlements granted by a one-off payment
-- (deleted_at filter was implicit via bun soft delete).
-- name: AuditOneOffPaymentEntitlementCount :one
SELECT count(*) FROM openrails.entitlements ent
WHERE ent.source_type = 'one_off'
  AND ent.source_id = sqlc.arg(source_id)
  AND ent.deleted_at IS NULL;

-- P-E-2
-- name: AuditOrphanOneOffEntitlements :many
SELECT
    ent.id AS ent_id,
    ent.customer_id::text AS user_id,
    ent.entitlement,
    ent.source_id,
    CASE WHEN purch.id IS NOT NULL THEN true ELSE false END AS payment_exists,
    purch.amount AS payment_amount
FROM openrails.entitlements ent
LEFT JOIN openrails.payments purch ON ent.source_id = purch.id
WHERE ent.source_type = 'one_off'
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND (ent.end_at IS NULL OR ent.end_at > NOW())
  AND (purch.id IS NULL OR purch.amount <= 0);

-- P-E-3
-- name: AuditRefundedPaymentActiveEntitlements :many
WITH refund_totals AS (
    SELECT
        refunded_payment_id,
        SUM(ABS(amount)) AS total_refunded
    FROM openrails.payments
    WHERE refunded_payment_id IS NOT NULL
    GROUP BY refunded_payment_id
)
SELECT
    purch.id AS payment_id,
    purch.customer_id::text AS user_id,
    purch.amount AS original_amount,
    COALESCE(rt.total_refunded, 0)::bigint AS refunded_amount,
    ent.id AS ent_id,
    ent.entitlement
FROM openrails.payments purch
LEFT JOIN refund_totals rt ON rt.refunded_payment_id = purch.id
INNER JOIN openrails.entitlements ent ON ent.source_id = purch.id
WHERE purch.subscription_id IS NULL
  AND purch.amount > 0
  AND COALESCE(rt.total_refunded, 0) >= purch.amount
  AND ent.source_type = 'one_off'
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND (ent.end_at IS NULL OR ent.end_at > NOW());

-- ============================================================================
-- Duplicate checks
-- ============================================================================

-- D-1
-- name: AuditMultipleActiveSubscriptions :many
SELECT
    customer_id::text AS user_id,
    COUNT(*)::int AS count,
    ARRAY_AGG(id ORDER BY created_at DESC)::uuid[] AS sub_ids
FROM openrails.subscriptions
WHERE status = 'active'
GROUP BY customer_id
HAVING COUNT(*) > 1;

-- D-2
-- name: AuditDuplicateChargesSamePeriod :many
WITH payment_products AS (
    SELECT
        purch.id,
        purch.customer_id,
        purch.amount,
        purch.purchased_at,
        price.product_id,
        prod.slug AS product_slug
    FROM openrails.payments purch
    JOIN openrails.prices price ON purch.price_id = price.id
    JOIN openrails.products prod ON price.product_id = prod.id
    WHERE purch.amount > 0
      AND purch.refunded_payment_id IS NULL
)
SELECT
    customer_id::text AS user_id,
    product_id,
    product_slug,
    COUNT(*)::int AS count,
    ARRAY_AGG(id ORDER BY purchased_at DESC)::uuid[] AS payment_ids,
    SUM(amount)::bigint AS total_amount,
    MIN(purchased_at)::timestamptz AS first_date,
    MAX(purchased_at)::timestamptz AS last_date
FROM payment_products
GROUP BY customer_id, product_id, product_slug, DATE_TRUNC('month', purchased_at)
HAVING COUNT(*) > 1;

-- D-3
-- name: AuditOverlappingEntitlementWindows :many
WITH active_entitlements AS (
    SELECT
        id,
        customer_id,
        entitlement,
        start_at,
        COALESCE(end_at, '9999-12-31'::timestamptz) AS end_at
    FROM openrails.entitlements
    WHERE revoked_at IS NULL
      AND deleted_at IS NULL
)
SELECT
    e1.customer_id::text AS user_id,
    e1.entitlement,
    (COUNT(DISTINCT e1.id) + COUNT(DISTINCT e2.id))::int AS count,
    (ARRAY_AGG(DISTINCT e1.id) || ARRAY_AGG(DISTINCT e2.id))::uuid[] AS ent_ids
FROM active_entitlements e1
JOIN active_entitlements e2 ON
    e1.customer_id = e2.customer_id
    AND e1.entitlement = e2.entitlement
    AND e1.id < e2.id
    AND e1.start_at < e2.end_at
    AND e2.start_at < e1.end_at
GROUP BY e1.customer_id, e1.entitlement;

-- ============================================================================
-- Subscription state checks
-- ============================================================================

-- SS-1
-- name: AuditActiveSubscriptionPastPeriodEnd :many
SELECT id, customer_id::text AS user_id, current_period_ends_at::timestamptz AS current_period_ends_at
FROM openrails.subscriptions
WHERE status = 'active'
  AND current_period_ends_at < NOW();

-- SS-2
-- name: AuditCancelledWithoutMetadata :many
SELECT id, customer_id::text AS user_id, cancelled_at, cancel_type, updated_at
FROM openrails.subscriptions
WHERE status = 'cancelled'
  AND (cancelled_at IS NULL OR cancel_type IS NULL);

-- SS-3
-- name: AuditPastDueWithoutRetry :many
SELECT id, customer_id::text AS user_id, retry_attempts
FROM openrails.subscriptions
WHERE status = 'past_due'
  AND next_retry_at IS NULL
  AND COALESCE(retry_attempts, 0) < 5;

-- SS-4
-- name: AuditInvalidPeriodDates :many
SELECT id, customer_id::text AS user_id,
       current_period_starts_at::timestamptz AS current_period_starts_at,
       current_period_ends_at::timestamptz AS current_period_ends_at
FROM openrails.subscriptions
WHERE current_period_starts_at IS NOT NULL
  AND current_period_ends_at IS NOT NULL
  AND current_period_starts_at >= current_period_ends_at;

-- SS-5
-- name: AuditEndedBeforeCancelled :many
SELECT id, customer_id::text AS user_id,
       ended_at::timestamptz AS ended_at,
       cancelled_at::timestamptz AS cancelled_at
FROM openrails.subscriptions
WHERE ended_at IS NOT NULL
  AND cancelled_at IS NOT NULL
  AND ended_at < cancelled_at;

-- ============================================================================
-- Entitlement state checks
-- ============================================================================

-- ES-1
-- name: AuditRevokedWithoutReason :many
SELECT id, customer_id::text AS user_id, entitlement, revoked_at::timestamptz AS revoked_at
FROM openrails.entitlements
WHERE revoked_at IS NOT NULL
  AND revoke_reason IS NULL
  AND deleted_at IS NULL;

-- ES-2
-- name: AuditReasonWithoutRevocation :many
SELECT id, customer_id::text AS user_id, entitlement, revoke_reason::text AS revoke_reason
FROM openrails.entitlements
WHERE revoke_reason IS NOT NULL
  AND revoked_at IS NULL
  AND deleted_at IS NULL;

-- ES-3
-- name: AuditInvalidTimeWindow :many
SELECT id, customer_id::text AS user_id, entitlement, start_at, end_at::timestamptz AS end_at
FROM openrails.entitlements
WHERE end_at IS NOT NULL
  AND start_at >= end_at
  AND deleted_at IS NULL;

-- ES-5
-- name: AuditMultipleIndefiniteEntitlements :many
SELECT
    customer_id::text AS user_id,
    entitlement,
    COUNT(*)::int AS count,
    ARRAY_AGG(id ORDER BY created_at DESC)::uuid[] AS ent_ids
FROM openrails.entitlements
WHERE end_at IS NULL
  AND revoked_at IS NULL
  AND deleted_at IS NULL
GROUP BY customer_id, entitlement
HAVING COUNT(*) > 1;

-- ============================================================================
-- Payment method checks
-- ============================================================================

-- PM-1
-- name: AuditActiveSubscriptionFailedPaymentMethod :many
SELECT
    sub.id AS sub_id,
    sub.customer_id::text AS user_id,
    pm.id AS pm_id,
    pm.failure_reason::text AS failure_reason
FROM openrails.subscriptions sub
JOIN openrails.payment_methods pm ON sub.payment_method_id = pm.id
WHERE sub.status = 'active'
  AND pm.failure_reason IS NOT NULL
  AND pm.failure_reason != '';

-- PM-2
-- name: AuditExpiredCardActiveSubscription :many
SELECT
    sub.id AS sub_id,
    sub.customer_id::text AS user_id,
    pm.id AS pm_id,
    pm.expiry_date::text AS expiry_date,
    pm.last_four,
    pm.card_type
FROM openrails.subscriptions sub
JOIN openrails.payment_methods pm ON sub.payment_method_id = pm.id
WHERE sub.status = 'active'
  AND pm.expiry_date IS NOT NULL
  AND TO_DATE(pm.expiry_date, 'MM/YY') < DATE_TRUNC('month', NOW());

-- PM-3
-- name: AuditOrphanPaymentMethodReference :many
SELECT sub.id AS sub_id, sub.customer_id::text AS user_id, sub.payment_method_id::uuid AS payment_method_id
FROM openrails.subscriptions sub
LEFT JOIN openrails.payment_methods pm ON sub.payment_method_id = pm.id
WHERE sub.payment_method_id IS NOT NULL
  AND pm.id IS NULL;

-- PM-4
-- name: AuditProcessorMismatch :many
SELECT
    sub.id AS sub_id,
    sub.customer_id::text AS user_id,
    sub.processor AS sub_processor,
    pm.processor::text AS pm_processor,
    pm.id AS pm_id
FROM openrails.subscriptions sub
JOIN openrails.payment_methods pm ON sub.payment_method_id = pm.id
WHERE sub.processor != pm.processor;

-- ============================================================================
-- Foreign key checks
-- ============================================================================

-- FK-1
-- name: AuditOrphanSubscriptionProduct :many
SELECT
    sub.id AS sub_id,
    sub.customer_id::text AS user_id,
    sub.product_id,
    CASE WHEN prod.id IS NOT NULL THEN true ELSE false END AS prod_exists,
    CASE WHEN prod.id IS NOT NULL THEN (prod.status = 'active') END AS prod_active
FROM openrails.subscriptions sub
LEFT JOIN openrails.products prod ON sub.product_id = prod.id
WHERE prod.id IS NULL OR prod.status <> 'active';

-- FK-2
-- name: AuditOrphanSubscriptionPrice :many
SELECT
    sub.id AS sub_id,
    sub.customer_id::text AS user_id,
    sub.price_id::uuid AS price_id,
    CASE WHEN price.id IS NOT NULL THEN true ELSE false END AS price_exists,
    CASE WHEN price.id IS NOT NULL THEN (price.status = 'active') END AS price_active
FROM openrails.subscriptions sub
LEFT JOIN openrails.prices price ON sub.price_id = price.id
WHERE price.id IS NULL OR price.status <> 'active';

-- FK-4
-- name: AuditPaymentOrphanSubscription :many
SELECT
    purch.id AS payment_id,
    purch.customer_id::text AS user_id,
    purch.subscription_id::uuid AS subscription_id
FROM openrails.payments purch
LEFT JOIN openrails.subscriptions sub ON purch.subscription_id = sub.id
WHERE purch.subscription_id IS NOT NULL
  AND sub.id IS NULL;

-- FK-5 (part 1): subscription sources
-- name: AuditEntitlementOrphanSubscriptionSource :many
SELECT
    ent.id AS ent_id,
    ent.customer_id::text AS user_id,
    ent.entitlement,
    ent.source_type,
    ent.source_id
FROM openrails.entitlements ent
LEFT JOIN openrails.subscriptions sub ON ent.source_id = sub.id
WHERE ent.source_type = 'subscription'
  AND ent.source_id IS NOT NULL
  AND ent.deleted_at IS NULL
  AND sub.id IS NULL;

-- FK-5 (part 2): one_off payment sources
-- name: AuditEntitlementOrphanPaymentSource :many
SELECT
    ent.id AS ent_id,
    ent.customer_id::text AS user_id,
    ent.entitlement,
    ent.source_type,
    ent.source_id
FROM openrails.entitlements ent
LEFT JOIN openrails.payments purch ON ent.source_id = purch.id
WHERE ent.source_type = 'one_off'
  AND ent.source_id IS NOT NULL
  AND ent.deleted_at IS NULL
  AND purch.id IS NULL;

-- ============================================================================
-- Admin grant checks
--
-- NOTE (schema drift, found during the sqlc migration): the bun-era queries
-- referenced entitlement_grants.entitlement / granted_at / expires_at / revoked_at,
-- none of which exist in openrails.entitlement_grants (it has price_id, granted_by,
-- reason, duration_days, created_at). The old AG-1/AG-3/AG-4 SQL could never
-- have executed. AG-1 and AG-4 are adapted to the real schema below (expiry
-- derived from created_at + duration_days); AG-3 (grant-level revocation) has
-- no equivalent column and was removed.
-- ============================================================================

-- AG-1: active (non-expired) admin grant with no corresponding entitlement.
-- name: AuditEntitlementGrantMissingEntitlements :many
SELECT
    ag.id AS grant_id,
    ag.customer_id::text AS user_id,
    ag.reason,
    ag.duration_days,
    ag.created_at AS granted_at
FROM openrails.entitlement_grants ag
LEFT JOIN openrails.entitlements ent ON
    ag.customer_id = ent.customer_id
    AND ent.source_type = 'admin'
    AND ent.source_id = ag.id
    AND ent.deleted_at IS NULL
WHERE (ag.duration_days IS NULL OR ag.duration_days = 0
       OR ag.created_at + make_interval(days => ag.duration_days) > NOW())
  AND ent.id IS NULL;

-- AG-2
-- name: AuditOrphanAdminEntitlements :many
SELECT
    ent.id AS ent_id,
    ent.customer_id::text AS user_id,
    ent.entitlement,
    ent.source_id
FROM openrails.entitlements ent
LEFT JOIN openrails.entitlement_grants ag ON ent.source_id = ag.id
WHERE ent.source_type = 'admin'
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ag.id IS NULL;

-- AG-4: grant expired (created_at + duration_days in the past) but the
-- entitlement it produced is still in effect.
-- name: AuditExpiredEntitlementGrantActiveEntitlement :many
SELECT
    ent.id AS ent_id,
    ent.customer_id::text AS user_id,
    ent.entitlement,
    ag.id AS grant_id,
    (ag.created_at + make_interval(days => ag.duration_days))::timestamptz AS expires_at
FROM openrails.entitlements ent
JOIN openrails.entitlement_grants ag ON ent.source_id = ag.id
WHERE ent.source_type = 'admin'
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND (ent.end_at IS NULL OR ent.end_at > NOW())
  AND ag.duration_days IS NOT NULL
  AND ag.duration_days > 0
  AND ag.created_at + make_interval(days => ag.duration_days) < NOW();

-- ============================================================================
-- Temporal checks
-- ============================================================================

-- T-1
-- name: AuditStalePendingSubscription :many
SELECT id, customer_id::text AS user_id,
       current_period_starts_at::timestamptz AS current_period_starts_at,
       created_at
FROM openrails.subscriptions
WHERE status = 'pending'
  AND current_period_starts_at IS NOT NULL
  AND current_period_starts_at <= NOW() - INTERVAL '24 hours';

-- T-2
-- name: AuditStalePastDueMaxRetries :many
SELECT id, customer_id::text AS user_id,
       retry_attempts::int AS retry_attempts,
       current_period_ends_at::timestamptz AS current_period_ends_at
FROM openrails.subscriptions
WHERE status = 'past_due'
  AND retry_attempts >= 5;

-- T-3
-- name: AuditFutureDatedPayment :many
SELECT id, customer_id::text AS user_id, purchased_at, amount
FROM openrails.payments
WHERE purchased_at > NOW() + INTERVAL '5 minutes';

-- T-4
-- name: AuditEntitlementDistantFutureStart :many
SELECT id, customer_id::text AS user_id, entitlement, start_at
FROM openrails.entitlements
WHERE start_at > NOW() + INTERVAL '1 year'
  AND deleted_at IS NULL;
