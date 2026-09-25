-- openrails.subscriptions. tier_group is set by the
-- trg_subscriptions_set_tier_group trigger — never written by the app.

-- name: CreateSubscription :execrows
INSERT INTO openrails.subscriptions (
    id, merchant_id, customer_id, product_id, price_id, scheduled_price_id,
    entitlements_spec_snapshot, status, started_at,
    ended_at, current_period_starts_at, current_period_ends_at, rail,
    rail_subscription_id, user_email, payment_method_id, last_retry_at,
    retry_attempts, next_retry_at, grace_ends_at, cancel_feedback,
    cancel_type, cancelled_at, deletion_scheduled_at, gateway_response,
    created_at, updated_at, psp_id, collection_policy
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, sqlc.narg(scheduled_price_id),
    sqlc.narg(entitlements_spec_snapshot),
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
    sqlc.arg(psp_id)::uuid,
    COALESCE(NULLIF(sqlc.arg(collection_policy)::text, ''), 'provider')
);

-- name: UpdateSubscriptionAt :execrows
-- Full-column update (nil pointers CLEAR fields like cancelled_at) against the
-- row version it was read at (#1102): a stale image never reverts a change.
UPDATE openrails.subscriptions SET
    price_id = $2,
    product_id = $3,
    entitlements_spec_snapshot = sqlc.narg(entitlements_spec_snapshot),
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
    transient_retries = sqlc.arg(transient_retries)::int,
    next_retry_at = sqlc.narg(next_retry_at),
    grace_ends_at = sqlc.narg(grace_ends_at),
    cancel_feedback = sqlc.narg(cancel_feedback),
    cancel_type = sqlc.narg(cancel_type),
    cancelled_at = sqlc.narg(cancelled_at),
    deletion_scheduled_at = sqlc.narg(deletion_scheduled_at),
    gateway_response = sqlc.narg(gateway_response),
    scheduled_price_id = sqlc.narg(scheduled_price_id),
    updated_at = sqlc.arg(updated_at)
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND row_version = sqlc.arg(expected_version)
  AND deleted_at IS NULL;

-- name: UpdateSubscriptionDecided :execrows
-- A lifecycle decision (#1091 part C): the full-row write that may change
-- status, paid period and cancellation, against the revision it was decided on.
-- Full-column update (the bun version listed every column explicitly so nil
-- pointers CLEAR fields like cancelled_at on reactivation).
UPDATE openrails.subscriptions SET
    price_id = $2,
    product_id = $3,
    entitlements_spec_snapshot = sqlc.narg(entitlements_spec_snapshot),
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
    transient_retries = sqlc.arg(transient_retries)::int,
    next_retry_at = sqlc.narg(next_retry_at),
    grace_ends_at = sqlc.narg(grace_ends_at),
    cancel_feedback = sqlc.narg(cancel_feedback),
    cancel_type = sqlc.narg(cancel_type),
    cancelled_at = sqlc.narg(cancelled_at),
    deletion_scheduled_at = sqlc.narg(deletion_scheduled_at),
    gateway_response = sqlc.narg(gateway_response),
    scheduled_price_id = sqlc.narg(scheduled_price_id),
    updated_at = sqlc.arg(updated_at),
    lifecycle_rev = lifecycle_rev + 1
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND lifecycle_rev = sqlc.arg(expected_rev)
  AND row_version = sqlc.arg(expected_version)
  AND deleted_at IS NULL
  -- The status-transition audit records this decision's name (0021).
  AND set_config('billing.decision', sqlc.arg(decision)::text, true) IS NOT NULL;

-- name: DeleteSubscription :execrows
DELETE FROM openrails.subscriptions WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND deleted_at IS NULL;

-- name: GetSubscriptionByID :one
SELECT * FROM openrails.subscriptions WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND deleted_at IS NULL;

-- name: GetSubscriptionByIDForUpdate :one
-- Lifecycle read-modify-writes hold this lock through their transaction.
SELECT * FROM openrails.subscriptions WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND deleted_at IS NULL
FOR UPDATE;

-- name: ListSubscriptionsByIDs :many
SELECT * FROM openrails.subscriptions WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
  AND deleted_at IS NULL;

-- name: GetLatestSubscriptionByCustomer :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at DESC
LIMIT 1;

-- name: GetSubscriptionByCustomerAndPrice :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1 AND sub.price_id = $2
  AND sub.deleted_at IS NULL
LIMIT 1;

-- name: GetLifecycleSubscriptionByCustomerAndProduct :one
-- NULLS FIRST prioritizes indefinite subscriptions.
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.product_id = $2
  AND sub.status IN ('active', 'pending', 'past_due', 'awaiting_method')
  AND sub.deleted_at IS NULL
ORDER BY sub.current_period_ends_at DESC NULLS FIRST
LIMIT 1;

-- name: GetActiveSubscriptionByCustomerAt :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.status = 'active'
  AND (sub.current_period_ends_at IS NULL OR sub.current_period_ends_at > sqlc.arg(now)::timestamptz)
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at DESC
LIMIT 1;

-- name: GetSubscriptionByPSPSubID :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.psp_id = sqlc.arg(psp_id)::uuid
  AND sub.rail = $1 AND sub.rail_subscription_id = $2
  AND sub.deleted_at IS NULL
LIMIT 1;

-- name: GetSubscriptionByPSPSubIDForUpdate :one
-- Row-locked variant for webhook apply read-modify-writes (#675): hold FOR
-- UPDATE across the read so a concurrent full-row UpdateAt can't clobber it.
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.psp_id = sqlc.arg(psp_id)::uuid
  AND sub.rail = $1 AND sub.rail_subscription_id = $2
  AND sub.deleted_at IS NULL
LIMIT 1
FOR UPDATE;

-- name: GetSubscriptionByPSPMetadataValue :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.psp_id = sqlc.arg(psp_id)::uuid
  AND sub.rail = $1
  AND sub.gateway_response ->> sqlc.arg(key)::text = sqlc.arg(value)::text
  AND sub.deleted_at IS NULL
LIMIT 1;

-- name: ListActiveSubscriptionsByCustomer :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid
  AND sub.customer_id = sqlc.arg(customer_id)::uuid
  AND sub.status = 'active'
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at DESC;

-- name: SetStripeSubscriptionPaymentMethod :execrows
-- Stripe provider truth owns the payment-method selection for Stripe-managed
-- subscriptions. The exact PSP predicate prevents one account's webhook from
-- relinking a sibling account's subscription.
UPDATE openrails.subscriptions SET
    payment_method_id = sqlc.narg(payment_method_id)::uuid,
    updated_at = now()
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND rail = 'stripe'
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail_subscription_id = sqlc.arg(rail_subscription_id)
  AND deleted_at IS NULL
  AND payment_method_id IS DISTINCT FROM sqlc.narg(payment_method_id)::uuid;

-- name: ClearStripePaymentMethodSubscriptions :execrows
-- A detached Stripe method can no longer be charged or reattached. Clear only
-- links owned by the exact PSP that delivered the detach event.
UPDATE openrails.subscriptions SET
    payment_method_id = NULL,
    updated_at = now()
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND rail = 'stripe'
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND deleted_at IS NULL;

-- name: ListActiveSubscriptionsForPSP :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.psp_id = sqlc.arg(psp_id)::uuid
  AND sub.rail = $1 AND sub.status = 'active'
  AND sub.deleted_at IS NULL;

-- #773: every active subscription pinned to one of a set of price rows — the
-- reprice_all_prior_versions(key, ...) match set (a key's prior-version price
-- ids). Uses idx_subscriptions_price_id.
-- name: ListActiveSubscriptionsByPriceIDs :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.price_id = ANY(sqlc.arg(price_ids)::uuid[]) AND sub.status = 'active'
  AND sub.deleted_at IS NULL;

-- name: CountSubscriptionsByCustomer :one
SELECT count(*) FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.deleted_at IS NULL;

-- name: ListSubscriptionsByCustomerPaged :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: CountSubscriptionsFiltered :one
SELECT count(*) FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND (sqlc.narg(customer_id)::uuid IS NULL OR sub.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(status)::text IS NULL OR sub.status::text = sqlc.narg(status)::text)
  AND (sqlc.narg(price_id)::uuid IS NULL OR sub.price_id = sqlc.narg(price_id)::uuid)
  AND (sqlc.narg(rail)::text IS NULL OR sub.rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(created_after)::timestamptz IS NULL OR sub.created_at >= sqlc.narg(created_after)::timestamptz)
  AND (sqlc.narg(created_before)::timestamptz IS NULL OR sub.created_at <= sqlc.narg(created_before)::timestamptz)
  AND (sqlc.narg(cancelled_after)::timestamptz IS NULL OR sub.cancelled_at >= sqlc.narg(cancelled_after)::timestamptz)
  AND (sqlc.narg(cancelled_before)::timestamptz IS NULL OR sub.cancelled_at <= sqlc.narg(cancelled_before)::timestamptz)
  AND (sqlc.narg(expires_before)::timestamptz IS NULL OR sub.current_period_ends_at <= sqlc.narg(expires_before)::timestamptz)
  AND sub.deleted_at IS NULL;

-- name: ListSubscriptionsFiltered :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND (sqlc.narg(customer_id)::uuid IS NULL OR sub.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(status)::text IS NULL OR sub.status::text = sqlc.narg(status)::text)
  AND (sqlc.narg(price_id)::uuid IS NULL OR sub.price_id = sqlc.narg(price_id)::uuid)
  AND (sqlc.narg(rail)::text IS NULL OR sub.rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(created_after)::timestamptz IS NULL OR sub.created_at >= sqlc.narg(created_after)::timestamptz)
  AND (sqlc.narg(created_before)::timestamptz IS NULL OR sub.created_at <= sqlc.narg(created_before)::timestamptz)
  AND (sqlc.narg(cancelled_after)::timestamptz IS NULL OR sub.cancelled_at >= sqlc.narg(cancelled_after)::timestamptz)
  AND (sqlc.narg(cancelled_before)::timestamptz IS NULL OR sub.cancelled_at <= sqlc.narg(cancelled_before)::timestamptz)
  AND (sqlc.narg(expires_before)::timestamptz IS NULL OR sub.current_period_ends_at <= sqlc.narg(expires_before)::timestamptz)
  AND sub.deleted_at IS NULL
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
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND prod.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.status IN ('active', 'pending', 'past_due', 'awaiting_method')
  AND prod.tier_group = $2
  AND sub.deleted_at IS NULL
ORDER BY sub.current_period_ends_at DESC NULLS FIRST
LIMIT 1;

-- #691 checkout guard: an `unknown` sub does NOT hold the lifecycle slot, but it
-- may still be alive (and billing) at the provider — a re-purchase would
-- double-bill. These lookups back the subscribe-time rejection.
-- name: GetUnknownSubscriptionByCustomerAndProduct :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.product_id = $2
  AND sub.status = 'unverified'
  AND sub.deleted_at IS NULL
ORDER BY sub.current_period_ends_at DESC NULLS FIRST
LIMIT 1;

-- name: GetUnknownSubscriptionByCustomerAndTierGroup :one
SELECT sub.* FROM openrails.subscriptions sub
JOIN openrails.products prod ON prod.id = sub.product_id
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND prod.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.status = 'unverified'
  AND prod.tier_group = $2
  AND sub.deleted_at IS NULL
ORDER BY sub.current_period_ends_at DESC NULLS FIRST
LIMIT 1;

-- #691 projection inversion: does this subscription project STANDING access
-- (open-ended entitlement window, closed only by proven events)? True for
-- auto-renew prices while the sub is non-terminal; terminal subs and bounded
-- (one-off/rental) prices keep bounded interval windows.
-- name: SubscriptionProjectsStandingAccess :one
SELECT EXISTS (
    SELECT 1 FROM openrails.subscriptions s
    JOIN openrails.prices p ON p.id = s.price_id AND p.merchant_id = s.merchant_id
    WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
      AND s.id = sqlc.arg(id)::uuid
      AND s.deleted_at IS NULL
      AND p.auto_renew
      AND NOT (s.collection_policy='engine' AND s.rail IN ('nmi','stripe'))
      AND s.status IN ('pending', 'active', 'past_due', 'awaiting_method', 'unverified')
) AS standing;

-- name: ListSubscriptionsByPaymentMethodIDs :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.payment_method_id = ANY(sqlc.arg(payment_method_ids)::uuid[])
  AND sub.deleted_at IS NULL;

-- name: MarkCancelledSubscriptionsSuperseded :execrows
-- Preserve cancelled subscriptions for refund/chargeback correlation while
-- stamping them superseded by the new activation (gateway_response patch).
UPDATE openrails.subscriptions
SET gateway_response = CASE WHEN jsonb_typeof(gateway_response) = 'object'
        THEN gateway_response || jsonb_build_object('superseded_at', current_timestamp, 'superseded_by_subscription_id', sqlc.narg(superseded_by)::text)
        ELSE jsonb_build_object('previous_gateway_response', gateway_response, 'superseded_at', current_timestamp, 'superseded_by_subscription_id', sqlc.narg(superseded_by)::text)
    END,
    updated_at = current_timestamp
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = $1
  AND product_id = $2
  AND status = 'cancelled'
  AND (sqlc.narg(exclude_id)::uuid IS NULL OR id != sqlc.narg(exclude_id)::uuid)
  AND deleted_at IS NULL;

-- CROSS-MERCHANT: merchants holding a due past_due subscription on the named
-- rails, through migration 0023's SECURITY DEFINER work queue (or#877 B5). The
-- dunning worker used to run ListDueDunningSubscriptions on the bare job
-- context; subscriptions FORCEs RLS, so the scan returned an empty slice and
-- scheduled dunning has never retried, parked or terminated anything. Ids only
-- — the due rows and every charge run per-merchant under RunInMerchantScope.
-- name: ListDueDunningMerchants :many
SELECT merchant_id FROM openrails.due_dunning_merchant_ids(
    sqlc.arg(rails)::text[],
    sqlc.arg(now)::timestamptz,
    sqlc.arg(merchant_limit)::int, sqlc.arg(include_engine)::boolean);

-- name: ListDueDunningSubscriptions :many
-- Dunning: past_due NMI-backed subscriptions whose next retry is due. Runs
-- inside one merchant's scope (see ListDueDunningMerchants above).
--
-- or#837: URGENCY ORDER + LIMIT. This was the flagship unbounded scan — no cap
-- at all, and each returned row can charge a card and terminate a subscription.
-- Most-overdue first, so a merchant whose backlog exceeds one pass retries the
-- subscriptions that have waited longest instead of an arbitrary slice; the
-- claim lease means the next pass picks up where this one stopped.
-- An engine renewal whose stored method is unusable is due too, so admission
-- routes it to awaiting_method (never the default card); a membership
-- awaiting a method past its dunning window is due to end.
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.rail = ANY(sqlc.arg(rails)::text[])
  AND ((sub.collection_policy <> 'engine' AND sub.rail='nmi' AND sub.status='past_due' AND sub.next_retry_at IS NOT NULL AND sub.next_retry_at <= sqlc.arg(now)::timestamptz)
       OR (sub.status='awaiting_method' AND sub.grace_ends_at <= sqlc.arg(now)::timestamptz
           AND ((sqlc.arg(include_engine)::boolean AND sub.collection_policy='engine') OR (sub.collection_policy='nmi_schedule' AND sub.rail='nmi')))
       OR (sqlc.arg(include_engine)::boolean AND sub.collection_policy='engine' AND sub.current_period_ends_at <= sqlc.arg(now)::timestamptz
           AND (sub.status='active' OR (sub.status='past_due' AND sub.next_retry_at <= sqlc.arg(now)::timestamptz))
           AND NOT EXISTS (SELECT 1 FROM openrails.rail_intents i WHERE i.merchant_id=sub.merchant_id AND i.subscription_id=sub.id AND i.intent_type='subscription_collection' AND i.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable'))))
  AND sub.deleted_at IS NULL
ORDER BY CASE WHEN sub.status='awaiting_method' THEN sub.grace_ends_at WHEN sub.collection_policy='engine' AND sub.status='active' THEN sub.current_period_ends_at ELSE sub.next_retry_at END, sub.id
LIMIT sqlc.arg(row_limit)::int;

-- name: GetLatestResumableCancelledSubscription :one
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.status = 'cancelled'
  AND (sub.current_period_ends_at IS NULL OR sub.current_period_ends_at > sqlc.arg(now)::timestamptz)
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at DESC
LIMIT 1;

-- #813: the plan-migration cohort — every subscription still billing (or
-- still being dunned) on the retired price. past_due is INCLUDED: a sub whose
-- dunning recovers would otherwise renew on the old plan and silently escape
-- the migration.
-- name: ListMigratableSubscriptionsByPriceID :many
SELECT * FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.price_id = sqlc.arg(price_id)::uuid
  AND sub.status IN ('active'::openrails.subscription_status, 'past_due'::openrails.subscription_status, 'awaiting_method'::openrails.subscription_status)
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at;

-- NMI deletion completion owns only this read-model marker. A full-row replay
-- can undo another command's price/card/period/quote while waiting for the lock.
-- name: ClearSubscriptionDeletionMarker :execrows
UPDATE openrails.subscriptions
SET deletion_scheduled_at=NULL, updated_at=sqlc.arg(now)::timestamptz
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
  AND psp_id=sqlc.arg(psp_id)::uuid
  AND rail_subscription_id=sqlc.arg(rail_subscription_id)::text
  AND deleted_at IS NULL
  AND deletion_scheduled_at IS NOT NULL;

-- name: GetInitialMembershipForUpdate :one
-- Accepted completion must see tombstones so it never resurrects a membership.
SELECT * FROM openrails.subscriptions
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
FOR UPDATE;

-- name: GetInitialMembershipForArchive :one
SELECT * FROM openrails.subscriptions
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid;

-- name: ListEngineSubscriptionsToWake :many
-- The delinquent engine memberships a replaced card retries at the next due
-- pass, and those awaiting a card (subscriptions.WakeForReplacedMethod).
SELECT id FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND collection_policy = 'engine'
  AND (status = 'awaiting_method' OR (status = 'past_due' AND (next_retry_at IS NULL OR next_retry_at > sqlc.arg(now)::timestamptz)))
  AND deleted_at IS NULL
ORDER BY id;
