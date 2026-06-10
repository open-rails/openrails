-- billing.subscriptions. tier_group is set by the
-- trg_subscriptions_set_tier_group trigger — never written by the app.

-- name: CreateSubscription :execrows
INSERT INTO billing.subscriptions (
    id, tenant_subject_id, product_id, price_id, scheduled_price_id,
    entitlements_spec_snapshot, credits_spec_snapshot, status, started_at,
    ended_at, current_period_starts_at, current_period_ends_at, processor,
    processor_subscription_id, user_email, payment_method_id, last_retry_at,
    retry_attempts, next_retry_at, grace_ends_at, cancel_feedback,
    cancel_type, cancelled_at, deletion_scheduled_at, gateway_response,
    created_at, updated_at
) VALUES (
    $1, $2, $3, $4, sqlc.narg(scheduled_price_id),
    sqlc.narg(entitlements_spec_snapshot), sqlc.narg(credits_spec_snapshot),
    COALESCE(NULLIF(sqlc.arg(status)::text, ''), 'pending')::billing.subscription_status,
    sqlc.arg(started_at),
    sqlc.narg(ended_at), sqlc.narg(current_period_starts_at), sqlc.narg(current_period_ends_at),
    sqlc.arg(processor), sqlc.arg(processor_subscription_id),
    sqlc.narg(user_email), sqlc.narg(payment_method_id), sqlc.narg(last_retry_at),
    sqlc.narg(retry_attempts), sqlc.narg(next_retry_at), sqlc.narg(grace_ends_at),
    sqlc.narg(cancel_feedback), sqlc.narg(cancel_type), sqlc.narg(cancelled_at),
    sqlc.narg(deletion_scheduled_at), sqlc.narg(gateway_response),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: UpdateSubscriptionAt :execrows
-- Full-column update (the bun version listed every column explicitly so nil
-- pointers CLEAR fields like cancelled_at on reactivation).
UPDATE billing.subscriptions SET
    price_id = $2,
    product_id = $3,
    entitlements_spec_snapshot = sqlc.narg(entitlements_spec_snapshot),
    credits_spec_snapshot = sqlc.narg(credits_spec_snapshot),
    status = sqlc.arg(status)::billing.subscription_status,
    started_at = sqlc.arg(started_at),
    ended_at = sqlc.narg(ended_at),
    current_period_starts_at = sqlc.narg(current_period_starts_at),
    current_period_ends_at = sqlc.narg(current_period_ends_at),
    processor = sqlc.arg(processor),
    processor_subscription_id = sqlc.arg(processor_subscription_id),
    user_email = sqlc.narg(user_email),
    payment_method_id = sqlc.narg(payment_method_id),
    last_retry_at = sqlc.narg(last_retry_at),
    retry_attempts = sqlc.narg(retry_attempts),
    next_retry_at = sqlc.narg(next_retry_at),
    grace_ends_at = sqlc.narg(grace_ends_at),
    cancel_feedback = sqlc.narg(cancel_feedback),
    cancel_type = sqlc.narg(cancel_type),
    cancelled_at = sqlc.narg(cancelled_at),
    gateway_response = sqlc.narg(gateway_response),
    scheduled_price_id = sqlc.narg(scheduled_price_id),
    updated_at = sqlc.arg(updated_at)
WHERE id = $1;

-- name: DeleteSubscription :execrows
DELETE FROM billing.subscriptions WHERE id = $1;

-- name: GetSubscriptionByID :one
SELECT * FROM billing.subscriptions WHERE id = $1;

-- name: ListSubscriptionsByIDs :many
SELECT * FROM billing.subscriptions WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: GetLatestSubscriptionByTenantSubject :one
SELECT * FROM billing.subscriptions sub
WHERE sub.tenant_subject_id = $1
ORDER BY sub.created_at DESC
LIMIT 1;

-- name: GetSubscriptionByTenantSubjectAndPrice :one
SELECT * FROM billing.subscriptions sub
WHERE sub.tenant_subject_id = $1 AND sub.price_id = $2
LIMIT 1;

-- name: GetLifecycleSubscriptionByTenantSubjectAndProduct :one
-- NULLS FIRST prioritizes indefinite subscriptions.
SELECT * FROM billing.subscriptions sub
WHERE sub.tenant_subject_id = $1
  AND sub.product_id = $2
  AND sub.status IN ('active', 'pending', 'past_due')
ORDER BY sub.current_period_ends_at DESC NULLS FIRST
LIMIT 1;

-- name: GetActiveSubscriptionByTenantSubjectAt :one
SELECT * FROM billing.subscriptions sub
WHERE sub.tenant_subject_id = $1
  AND sub.status = 'active'
  AND (sub.current_period_ends_at IS NULL OR sub.current_period_ends_at > sqlc.arg(now)::timestamptz)
ORDER BY sub.created_at DESC
LIMIT 1;

-- name: GetSubscriptionByProcessorSubID :one
SELECT * FROM billing.subscriptions sub
WHERE sub.processor = $1 AND sub.processor_subscription_id = $2
LIMIT 1;

-- name: GetSubscriptionByProcessorMetadataValue :one
SELECT * FROM billing.subscriptions sub
WHERE sub.processor = $1
  AND sub.gateway_response ->> sqlc.arg(key)::text = sqlc.arg(value)::text
LIMIT 1;

-- name: ListActiveSubscriptionsByTenantSubject :many
SELECT * FROM billing.subscriptions sub
WHERE sub.tenant_subject_id = $1 AND sub.status = 'active'
ORDER BY sub.created_at DESC;

-- name: ListSubscriptionsByTenantSubjectProcessor :many
SELECT * FROM billing.subscriptions sub
WHERE sub.tenant_subject_id = $1 AND sub.processor = $2
ORDER BY sub.created_at DESC;

-- name: ListActiveSubscriptionsByProcessor :many
SELECT * FROM billing.subscriptions sub
WHERE sub.processor = $1 AND sub.status = 'active';

-- name: CountSubscriptionsByTenantSubject :one
SELECT count(*) FROM billing.subscriptions sub
WHERE sub.tenant_subject_id = $1;

-- name: ListSubscriptionsByTenantSubjectPaged :many
SELECT * FROM billing.subscriptions sub
WHERE sub.tenant_subject_id = $1
ORDER BY sub.created_at DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: CountSubscriptionsFiltered :one
SELECT count(*) FROM billing.subscriptions sub
WHERE (sqlc.narg(tenant_subject_id)::uuid IS NULL OR sub.tenant_subject_id = sqlc.narg(tenant_subject_id)::uuid)
  AND (sqlc.narg(status)::text IS NULL OR sub.status::text = sqlc.narg(status)::text)
  AND (sqlc.narg(price_id)::uuid IS NULL OR sub.price_id = sqlc.narg(price_id)::uuid)
  AND (sqlc.narg(processor)::text IS NULL OR sub.processor = sqlc.narg(processor)::text)
  AND (sqlc.narg(created_after)::timestamptz IS NULL OR sub.created_at >= sqlc.narg(created_after)::timestamptz)
  AND (sqlc.narg(created_before)::timestamptz IS NULL OR sub.created_at <= sqlc.narg(created_before)::timestamptz)
  AND (sqlc.narg(cancelled_after)::timestamptz IS NULL OR sub.cancelled_at >= sqlc.narg(cancelled_after)::timestamptz)
  AND (sqlc.narg(cancelled_before)::timestamptz IS NULL OR sub.cancelled_at <= sqlc.narg(cancelled_before)::timestamptz)
  AND (sqlc.narg(expires_before)::timestamptz IS NULL OR sub.current_period_ends_at <= sqlc.narg(expires_before)::timestamptz);

-- name: ListSubscriptionsFiltered :many
SELECT * FROM billing.subscriptions sub
WHERE (sqlc.narg(tenant_subject_id)::uuid IS NULL OR sub.tenant_subject_id = sqlc.narg(tenant_subject_id)::uuid)
  AND (sqlc.narg(status)::text IS NULL OR sub.status::text = sqlc.narg(status)::text)
  AND (sqlc.narg(price_id)::uuid IS NULL OR sub.price_id = sqlc.narg(price_id)::uuid)
  AND (sqlc.narg(processor)::text IS NULL OR sub.processor = sqlc.narg(processor)::text)
  AND (sqlc.narg(created_after)::timestamptz IS NULL OR sub.created_at >= sqlc.narg(created_after)::timestamptz)
  AND (sqlc.narg(created_before)::timestamptz IS NULL OR sub.created_at <= sqlc.narg(created_before)::timestamptz)
  AND (sqlc.narg(cancelled_after)::timestamptz IS NULL OR sub.cancelled_at >= sqlc.narg(cancelled_after)::timestamptz)
  AND (sqlc.narg(cancelled_before)::timestamptz IS NULL OR sub.cancelled_at <= sqlc.narg(cancelled_before)::timestamptz)
  AND (sqlc.narg(expires_before)::timestamptz IS NULL OR sub.current_period_ends_at <= sqlc.narg(expires_before)::timestamptz)
ORDER BY
    CASE WHEN sqlc.arg(sort_by)::text = 'expires_at'   AND NOT sqlc.arg(sort_desc)::boolean THEN sub.current_period_ends_at END ASC,
    CASE WHEN sqlc.arg(sort_by)::text = 'expires_at'   AND sqlc.arg(sort_desc)::boolean     THEN sub.current_period_ends_at END DESC,
    CASE WHEN sqlc.arg(sort_by)::text = 'cancelled_at' AND NOT sqlc.arg(sort_desc)::boolean THEN sub.cancelled_at END ASC,
    CASE WHEN sqlc.arg(sort_by)::text = 'cancelled_at' AND sqlc.arg(sort_desc)::boolean     THEN sub.cancelled_at END DESC,
    CASE WHEN sqlc.arg(sort_by)::text = 'created_at'   AND NOT sqlc.arg(sort_desc)::boolean THEN sub.created_at END ASC,
    CASE WHEN sqlc.arg(sort_by)::text = 'created_at'   AND sqlc.arg(sort_desc)::boolean     THEN sub.created_at END DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: GetLifecycleSubscriptionByTenantSubjectAndTierGroup :one
SELECT sub.* FROM billing.subscriptions sub
JOIN billing.products prod ON prod.id = sub.product_id
WHERE sub.tenant_subject_id = $1
  AND sub.status IN ('active', 'pending', 'past_due')
  AND prod.tier_group = $2
ORDER BY sub.current_period_ends_at DESC NULLS FIRST
LIMIT 1;

-- name: ListSubscriptionsByPaymentMethodIDs :many
SELECT * FROM billing.subscriptions sub
WHERE sub.payment_method_id = ANY(sqlc.arg(payment_method_ids)::uuid[]);
