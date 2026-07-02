-- openrails.subscriptions. tier_group is set by the
-- trg_subscriptions_set_tier_group trigger — never written by the app.

-- name: CreateSubscription :execrows
INSERT INTO openrails.subscriptions (
    id, merchant_id, customer_id, product_id, price_id, scheduled_price_id,
    entitlements_spec_snapshot, credits_spec_snapshot, status, started_at,
    ended_at, current_period_starts_at, current_period_ends_at, rail,
    rail_subscription_id, user_email, payment_method_id, last_retry_at,
    retry_attempts, next_retry_at, grace_ends_at, cancel_feedback,
    cancel_type, cancelled_at, deletion_scheduled_at, gateway_response,
    created_at, updated_at, provider_account_id
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, sqlc.narg(scheduled_price_id),
    sqlc.narg(entitlements_spec_snapshot), sqlc.narg(credits_spec_snapshot),
    COALESCE(NULLIF(sqlc.arg(status)::text, ''), 'pending')::openrails.subscription_status,
    sqlc.arg(started_at),
    sqlc.narg(ended_at), sqlc.narg(current_period_starts_at), sqlc.narg(current_period_ends_at),
    sqlc.arg(rail), sqlc.arg(rail_subscription_id),
    sqlc.narg(user_email), sqlc.narg(payment_method_id), sqlc.narg(last_retry_at),
    sqlc.narg(retry_attempts), sqlc.narg(next_retry_at), sqlc.narg(grace_ends_at),
    sqlc.narg(cancel_feedback), sqlc.narg(cancel_type), sqlc.narg(cancelled_at),
    sqlc.narg(deletion_scheduled_at), sqlc.narg(gateway_response),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.narg(provider_account_id)
);

-- name: UpdateSubscriptionAt :execrows
-- Full-column update (the bun version listed every column explicitly so nil
-- pointers CLEAR fields like cancelled_at on reactivation).
UPDATE openrails.subscriptions SET
    price_id = $2,
    product_id = $3,
    entitlements_spec_snapshot = sqlc.narg(entitlements_spec_snapshot),
    credits_spec_snapshot = sqlc.narg(credits_spec_snapshot),
    status = sqlc.arg(status)::openrails.subscription_status,
    started_at = sqlc.arg(started_at),
    ended_at = sqlc.narg(ended_at),
    current_period_starts_at = sqlc.narg(current_period_starts_at),
    current_period_ends_at = sqlc.narg(current_period_ends_at),
    rail = sqlc.arg(rail),
    rail_subscription_id = sqlc.arg(rail_subscription_id),
    user_email = sqlc.narg(user_email),
    payment_method_id = sqlc.narg(payment_method_id),
    last_retry_at = sqlc.narg(last_retry_at),
    retry_attempts = sqlc.narg(retry_attempts),
    next_retry_at = sqlc.narg(next_retry_at),
    grace_ends_at = sqlc.narg(grace_ends_at),
    cancel_feedback = sqlc.narg(cancel_feedback),
    cancel_type = sqlc.narg(cancel_type),
    cancelled_at = sqlc.narg(cancelled_at),
    deletion_scheduled_at = sqlc.narg(deletion_scheduled_at),
    gateway_response = sqlc.narg(gateway_response),
    scheduled_price_id = sqlc.narg(scheduled_price_id),
    updated_at = sqlc.arg(updated_at)
WHERE id = $1;

-- name: DeleteSubscription :execrows
DELETE FROM openrails.subscriptions WHERE id = $1;

-- name: GetSubscriptionByID :one
SELECT * FROM openrails.subscriptions WHERE id = $1;

-- name: ListSubscriptionsByIDs :many
SELECT * FROM openrails.subscriptions WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: GetLatestSubscriptionByCustomer :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.customer_id = $1
ORDER BY sub.created_at DESC
LIMIT 1;

-- name: GetSubscriptionByCustomerAndPrice :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.customer_id = $1 AND sub.price_id = $2
LIMIT 1;

-- name: GetLifecycleSubscriptionByCustomerAndProduct :one
-- NULLS FIRST prioritizes indefinite subscriptions.
SELECT * FROM openrails.subscriptions sub
WHERE sub.customer_id = $1
  AND sub.product_id = $2
  AND sub.status IN ('active', 'pending', 'past_due')
ORDER BY sub.current_period_ends_at DESC NULLS FIRST
LIMIT 1;

-- name: GetActiveSubscriptionByCustomerAt :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.customer_id = $1
  AND sub.status = 'active'
  AND (sub.current_period_ends_at IS NULL OR sub.current_period_ends_at > sqlc.arg(now)::timestamptz)
ORDER BY sub.created_at DESC
LIMIT 1;

-- name: GetSubscriptionByRailSubID :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.rail = $1 AND sub.rail_subscription_id = $2
LIMIT 1;

-- name: GetSubscriptionByRailSubIDForUpdate :one
-- Row-locked variant for webhook apply read-modify-writes (#675): hold FOR
-- UPDATE across the read so a concurrent full-row UpdateAt can't clobber it.
SELECT * FROM openrails.subscriptions sub
WHERE sub.rail = $1 AND sub.rail_subscription_id = $2
LIMIT 1
FOR UPDATE;

-- name: GetSubscriptionByRailMetadataValue :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.rail = $1
  AND sub.gateway_response ->> sqlc.arg(key)::text = sqlc.arg(value)::text
LIMIT 1;

-- name: ListActiveSubscriptionsByCustomer :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.customer_id = $1 AND sub.status = 'active'
ORDER BY sub.created_at DESC;

-- name: ListSubscriptionsByCustomerRail :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.customer_id = $1 AND sub.rail = $2
ORDER BY sub.created_at DESC;

-- name: ListActiveSubscriptionsByRail :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.rail = $1 AND sub.status = 'active';

-- name: CountSubscriptionsByCustomer :one
SELECT count(*) FROM openrails.subscriptions sub
WHERE sub.customer_id = $1;

-- name: ListSubscriptionsByCustomerPaged :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.customer_id = $1
ORDER BY sub.created_at DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: CountSubscriptionsFiltered :one
SELECT count(*) FROM openrails.subscriptions sub
WHERE (sqlc.narg(customer_id)::uuid IS NULL OR sub.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(status)::text IS NULL OR sub.status::text = sqlc.narg(status)::text)
  AND (sqlc.narg(price_id)::uuid IS NULL OR sub.price_id = sqlc.narg(price_id)::uuid)
  AND (sqlc.narg(rail)::text IS NULL OR sub.rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(created_after)::timestamptz IS NULL OR sub.created_at >= sqlc.narg(created_after)::timestamptz)
  AND (sqlc.narg(created_before)::timestamptz IS NULL OR sub.created_at <= sqlc.narg(created_before)::timestamptz)
  AND (sqlc.narg(cancelled_after)::timestamptz IS NULL OR sub.cancelled_at >= sqlc.narg(cancelled_after)::timestamptz)
  AND (sqlc.narg(cancelled_before)::timestamptz IS NULL OR sub.cancelled_at <= sqlc.narg(cancelled_before)::timestamptz)
  AND (sqlc.narg(expires_before)::timestamptz IS NULL OR sub.current_period_ends_at <= sqlc.narg(expires_before)::timestamptz);

-- name: ListSubscriptionsFiltered :many
SELECT * FROM openrails.subscriptions sub
WHERE (sqlc.narg(customer_id)::uuid IS NULL OR sub.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(status)::text IS NULL OR sub.status::text = sqlc.narg(status)::text)
  AND (sqlc.narg(price_id)::uuid IS NULL OR sub.price_id = sqlc.narg(price_id)::uuid)
  AND (sqlc.narg(rail)::text IS NULL OR sub.rail = sqlc.narg(rail)::text)
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

-- name: GetLifecycleSubscriptionByCustomerAndTierGroup :one
SELECT sub.* FROM openrails.subscriptions sub
JOIN openrails.products prod ON prod.id = sub.product_id
WHERE sub.customer_id = $1
  AND sub.status IN ('active', 'pending', 'past_due')
  AND prod.tier_group = $2
ORDER BY sub.current_period_ends_at DESC NULLS FIRST
LIMIT 1;

-- name: ListSubscriptionsByPaymentMethodIDs :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.payment_method_id = ANY(sqlc.arg(payment_method_ids)::uuid[]);

-- name: MarkCancelledSubscriptionsSuperseded :execrows
-- Preserve cancelled subscriptions for refund/chargeback correlation while
-- stamping them superseded by the new activation (gateway_response patch).
UPDATE openrails.subscriptions
SET gateway_response = CASE WHEN jsonb_typeof(gateway_response) = 'object'
        THEN gateway_response || jsonb_build_object('superseded_at', current_timestamp, 'superseded_by_subscription_id', sqlc.narg(superseded_by)::text)
        ELSE jsonb_build_object('previous_gateway_response', gateway_response, 'superseded_at', current_timestamp, 'superseded_by_subscription_id', sqlc.narg(superseded_by)::text)
    END,
    updated_at = current_timestamp
WHERE customer_id = $1
  AND product_id = $2
  AND status = 'cancelled'
  AND (sqlc.narg(exclude_id)::uuid IS NULL OR id != sqlc.narg(exclude_id)::uuid);

-- name: ListDueDunningSubscriptions :many
-- Dunning: past_due NMI-backed subscriptions whose next retry is due.
SELECT * FROM openrails.subscriptions sub
WHERE sub.rail = ANY(sqlc.arg(rails)::text[])
  AND sub.status = 'past_due'
  AND sub.next_retry_at IS NOT NULL AND sub.next_retry_at <= sqlc.arg(now)::timestamptz;

-- name: ClaimDunningAttempt :execrows
-- Lease-style claim: pushes next_retry_at out so concurrent dunning runs
-- cannot double-charge; only claims a still-due past_due row.
UPDATE openrails.subscriptions
SET next_retry_at = sqlc.arg(lease_until)::timestamptz,
    last_retry_at = sqlc.arg(claimed_at)::timestamptz,
    updated_at = sqlc.arg(claimed_at)::timestamptz
WHERE id = $1
  AND status = 'past_due'
  AND next_retry_at IS NOT NULL AND next_retry_at <= sqlc.arg(claimed_at)::timestamptz;

-- name: ListPendingDeletionScheduledSubscriptions :many
-- Boot rescan (#344 follow-up): cancelled subscriptions still carrying the
-- deletion_scheduled_at marker — their deferred rail-side delete never
-- finalized (the deletion kill switch skipped it, or the job was lost). The
-- worker-startup rescan re-enqueues these via the deferred-delete scheduler.
SELECT * FROM openrails.subscriptions sub
WHERE sub.status = 'cancelled'
  AND sub.deletion_scheduled_at IS NOT NULL;

-- name: ListSilentLapsedSubscriptions :many
-- Subscription liveness sync (#367): the SILENT lapsed cohort — still-active
-- subscriptions on provider-truth-probeable rails whose period lapsed
-- past the probe slack, or whose imported period evidence is missing, with NO
-- signal either way: no renewal (the period would have advanced), no failure
-- webhook (status would be past_due — that cohort is dunning's, excluded here),
-- and no payment recorded at/after the period end when a period end exists.
-- Re-derived every pass, so an unreachable provider needs no durable read-queue:
-- unchanged state simply reappears next cycle.
SELECT * FROM openrails.subscriptions sub
WHERE sub.rail = ANY(sqlc.arg(rails)::text[])
  AND sub.status = 'active'
  AND (
    sub.current_period_ends_at IS NULL
    OR sub.current_period_ends_at <= sqlc.arg(cutoff)::timestamptz
  )
  AND NOT EXISTS (
    SELECT 1 FROM openrails.payments p
    WHERE p.subscription_id = sub.id
      AND p.status = 'completed'
      AND (
        sub.current_period_ends_at IS NULL
        OR p.purchased_at >= sub.current_period_ends_at
      )
  );

-- name: GetLatestResumableCancelledSubscription :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.customer_id = $1
  AND sub.status = 'cancelled'
  AND (sub.current_period_ends_at IS NULL OR sub.current_period_ends_at > sqlc.arg(now)::timestamptz)
ORDER BY sub.created_at DESC
LIMIT 1;
