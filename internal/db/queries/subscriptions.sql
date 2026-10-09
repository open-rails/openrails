-- billing.subscriptions. tier_group is set by the
-- trg_subscriptions_set_tier_group trigger — never written by the app.

-- name: CreateSubscription :execrows
INSERT INTO billing.subscriptions (
    id, merchant_id, customer_id, product_id, price_id, scheduled_price_id,
    access_duration_hours_snapshot, status, started_at,
    ended_at, current_period_starts_at, current_period_ends_at, rail,
    rail_subscription_id, payment_method_id, last_retry_at,
    retry_attempts, next_retry_at, grace_ends_at, cancel_feedback,
    cancel_type, canceled_at, deletion_scheduled_at, gateway_response,
    created_at, updated_at, psp_id, collection_policy
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, sqlc.narg(scheduled_price_id),
    sqlc.narg(access_duration_hours_snapshot)::int,
    COALESCE(NULLIF(sqlc.arg(status)::text, ''), 'pending'),
    sqlc.arg(started_at),
    sqlc.narg(ended_at), sqlc.narg(current_period_starts_at), sqlc.narg(current_period_ends_at),
    sqlc.arg(rail), NULLIF(sqlc.arg(rail_subscription_id)::text, ''),
    sqlc.narg(payment_method_id), sqlc.narg(last_retry_at),
    sqlc.narg(retry_attempts), sqlc.narg(next_retry_at), sqlc.narg(grace_ends_at),
    sqlc.narg(cancel_feedback), sqlc.narg(cancel_type), sqlc.narg(canceled_at),
    sqlc.narg(deletion_scheduled_at), sqlc.narg(gateway_response),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.arg(psp_id)::uuid,
    COALESCE(NULLIF(sqlc.arg(collection_policy)::text, ''), 'provider')
);

-- name: UpdateSubscriptionAt :execrows
-- Full-column update (nil pointers CLEAR fields like canceled_at) against the
-- row version it was read at (#1102): a stale image never reverts a change.
UPDATE billing.subscriptions SET
    price_id = $2,
    product_id = $3,
    access_duration_hours_snapshot = sqlc.narg(access_duration_hours_snapshot)::int,
    status = sqlc.arg(status),
    started_at = sqlc.arg(started_at),
    ended_at = sqlc.narg(ended_at),
    current_period_starts_at = sqlc.narg(current_period_starts_at),
    current_period_ends_at = sqlc.narg(current_period_ends_at),
    rail = sqlc.arg(rail),
    rail_subscription_id = NULLIF(sqlc.arg(rail_subscription_id)::text, ''),
    payment_method_id = sqlc.narg(payment_method_id),
    last_retry_at = sqlc.narg(last_retry_at),
    retry_attempts = sqlc.narg(retry_attempts),
    transient_retries = sqlc.arg(transient_retries)::int,
    next_retry_at = sqlc.narg(next_retry_at),
    grace_ends_at = sqlc.narg(grace_ends_at),
    cancel_feedback = sqlc.narg(cancel_feedback),
    cancel_type = sqlc.narg(cancel_type),
    canceled_at = sqlc.narg(canceled_at),
    deletion_scheduled_at = sqlc.narg(deletion_scheduled_at),
    gateway_response = sqlc.narg(gateway_response),
    scheduled_price_id = sqlc.narg(scheduled_price_id),
    dunning_policy = sqlc.narg(dunning_policy)::jsonb,
    updated_at = sqlc.arg(updated_at)
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND row_version = sqlc.arg(expected_version)
  AND deleted_at IS NULL;

-- name: UpdateSubscriptionDecided :execrows
-- A lifecycle decision (#1091 part C): the full-row write that may change
-- status, paid period and cancellation, against the revision it was decided on.
-- Full-column update (the bun version listed every column explicitly so nil
-- pointers CLEAR fields like canceled_at on reactivation).
UPDATE billing.subscriptions SET
    price_id = $2,
    product_id = $3,
    access_duration_hours_snapshot = sqlc.narg(access_duration_hours_snapshot)::int,
    status = sqlc.arg(status),
    started_at = sqlc.arg(started_at),
    ended_at = sqlc.narg(ended_at),
    current_period_starts_at = sqlc.narg(current_period_starts_at),
    current_period_ends_at = sqlc.narg(current_period_ends_at),
    rail = sqlc.arg(rail),
    rail_subscription_id = NULLIF(sqlc.arg(rail_subscription_id)::text, ''),
    payment_method_id = sqlc.narg(payment_method_id),
    last_retry_at = sqlc.narg(last_retry_at),
    retry_attempts = sqlc.narg(retry_attempts),
    transient_retries = sqlc.arg(transient_retries)::int,
    next_retry_at = sqlc.narg(next_retry_at),
    grace_ends_at = sqlc.narg(grace_ends_at),
    cancel_feedback = sqlc.narg(cancel_feedback),
    cancel_type = sqlc.narg(cancel_type),
    canceled_at = sqlc.narg(canceled_at),
    deletion_scheduled_at = sqlc.narg(deletion_scheduled_at),
    gateway_response = sqlc.narg(gateway_response),
    scheduled_price_id = sqlc.narg(scheduled_price_id),
    dunning_policy = sqlc.narg(dunning_policy)::jsonb,
    updated_at = sqlc.arg(updated_at),
    lifecycle_rev = lifecycle_rev + 1
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND lifecycle_rev = sqlc.arg(expected_rev)
  AND row_version = sqlc.arg(expected_version)
  AND deleted_at IS NULL
  -- The status-transition audit records this decision's name (0021).
  AND set_config('openrails.subscription_decision', sqlc.arg(decision)::text, true) IS NOT NULL;

-- name: GetSubscriptionByID :one
SELECT * FROM billing.subscriptions WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND deleted_at IS NULL;

-- name: GetSubscriptionByIDForUpdate :one
-- Lifecycle read-modify-writes hold this lock through their transaction.
SELECT * FROM billing.subscriptions WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND deleted_at IS NULL
FOR UPDATE;

-- name: ListSubscriptionsByIDs :many
SELECT * FROM billing.subscriptions WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
  AND deleted_at IS NULL;

-- name: GetLatestSubscriptionByCustomer :one
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at DESC
LIMIT 1;

-- name: GetSubscriptionByCustomerAndPrice :one
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1 AND sub.price_id = $2
  AND sub.deleted_at IS NULL
LIMIT 1;

-- name: GetLifecycleSubscriptionByCustomerAndProduct :one
-- NULLS FIRST prioritizes indefinite subscriptions.
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.product_id = $2
  AND sub.status IN ('active', 'pending', 'past_due', 'awaiting_method')
  AND sub.deleted_at IS NULL
ORDER BY sub.current_period_ends_at DESC NULLS FIRST
LIMIT 1;

-- name: GetActiveSubscriptionByCustomerAt :one
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.status = 'active'
  AND (sub.current_period_ends_at IS NULL OR sub.current_period_ends_at > sqlc.arg(now)::timestamptz)
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at DESC
LIMIT 1;

-- name: GetSubscriptionByPSPSubID :one
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.psp_id = sqlc.arg(psp_id)::uuid
  AND sub.rail = $1 AND sub.rail_subscription_id = sqlc.arg(rail_subscription_id)::text
  AND sub.deleted_at IS NULL
LIMIT 1;

-- name: GetSubscriptionByPSPSubIDForUpdate :one
-- Row-locked variant for webhook apply read-modify-writes (#675): hold FOR
-- UPDATE across the read so a concurrent full-row UpdateAt can't clobber it.
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.psp_id = sqlc.arg(psp_id)::uuid
  AND sub.rail = $1 AND sub.rail_subscription_id = sqlc.arg(rail_subscription_id)::text
  AND sub.deleted_at IS NULL
LIMIT 1
FOR UPDATE;

-- name: GetSubscriptionByGatewayOrder :one
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.psp_id = sqlc.arg(psp_id)::uuid
  AND sub.rail = sqlc.arg(rail)::text
  AND sub.gateway_response ->> 'order_id' = sqlc.arg(order_id)::text
  AND sub.deleted_at IS NULL
LIMIT 1;

-- name: ListActiveSubscriptionsByCustomer :many
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid
  AND sub.customer_id = sqlc.arg(customer_id)::uuid
  AND sub.status = 'active'
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at DESC;

-- name: SetStripeSubscriptionPaymentMethod :execrows
-- Stripe provider truth owns the payment-method selection for Stripe-managed
-- subscriptions. The exact PSP predicate prevents one account's webhook from
-- relinking a sibling account's subscription.
UPDATE billing.subscriptions SET
    payment_method_id = sqlc.narg(payment_method_id)::uuid,
    updated_at = now()
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND rail = 'stripe'
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail_subscription_id = sqlc.arg(rail_subscription_id)::text
  AND deleted_at IS NULL
  AND payment_method_id IS DISTINCT FROM sqlc.narg(payment_method_id)::uuid;

-- name: ClearStripePaymentMethodSubscriptions :execrows
-- A detached Stripe method can no longer be charged or reattached. Clear only
-- links owned by the exact PSP that delivered the detach event.
UPDATE billing.subscriptions SET
    payment_method_id = NULL,
    updated_at = now()
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND rail = 'stripe'
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND deleted_at IS NULL;

-- name: ListActiveSubscriptionsForPSP :many
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.psp_id = sqlc.arg(psp_id)::uuid
  AND sub.rail = $1 AND sub.status = 'active'
  AND sub.deleted_at IS NULL;

-- #773: every active subscription pinned to one of a set of price rows — the
-- reprice_all_prior_versions(key, ...) match set (a key's prior-version price
-- ids). Uses subscriptions_price_id_idx.
-- name: ListActiveSubscriptionsByPriceIDs :many
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.price_id = ANY(sqlc.arg(price_ids)::uuid[]) AND sub.status = 'active'
  AND sub.deleted_at IS NULL;

-- name: CountSubscriptionsByCustomer :one
SELECT count(*) FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.deleted_at IS NULL;

-- name: ListSubscriptionsByCustomerPaged :many
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: CountSubscriptionsFiltered :one
SELECT count(*) FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND (sqlc.narg(customer_id)::uuid IS NULL OR sub.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(status)::text IS NULL OR sub.status::text = sqlc.narg(status)::text)
  AND (sqlc.narg(price_id)::uuid IS NULL OR sub.price_id = sqlc.narg(price_id)::uuid)
  AND (sqlc.narg(rail)::text IS NULL OR sub.rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(created_after)::timestamptz IS NULL OR sub.created_at >= sqlc.narg(created_after)::timestamptz)
  AND (sqlc.narg(created_before)::timestamptz IS NULL OR sub.created_at <= sqlc.narg(created_before)::timestamptz)
  AND (sqlc.narg(canceled_after)::timestamptz IS NULL OR sub.canceled_at >= sqlc.narg(canceled_after)::timestamptz)
  AND (sqlc.narg(canceled_before)::timestamptz IS NULL OR sub.canceled_at <= sqlc.narg(canceled_before)::timestamptz)
  AND (sqlc.narg(expires_before)::timestamptz IS NULL OR sub.current_period_ends_at <= sqlc.narg(expires_before)::timestamptz)
  AND sub.deleted_at IS NULL;

-- One page of a subscription list, newest first; after_at/after_id is the
-- last row of the previous page.
-- name: ListSubscriptionsPage :many
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND (sqlc.narg(customer_id)::uuid IS NULL OR sub.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(status)::text IS NULL OR sub.status::text = sqlc.narg(status)::text)
  AND (sqlc.narg(price_id)::uuid IS NULL OR sub.price_id = sqlc.narg(price_id)::uuid)
  AND (sqlc.narg(rail)::text IS NULL OR sub.rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(created_after)::timestamptz IS NULL OR sub.created_at >= sqlc.narg(created_after)::timestamptz)
  AND (sqlc.narg(created_before)::timestamptz IS NULL OR sub.created_at <= sqlc.narg(created_before)::timestamptz)
  AND (sqlc.narg(canceled_after)::timestamptz IS NULL OR sub.canceled_at >= sqlc.narg(canceled_after)::timestamptz)
  AND (sqlc.narg(canceled_before)::timestamptz IS NULL OR sub.canceled_at <= sqlc.narg(canceled_before)::timestamptz)
  AND (sqlc.narg(expires_before)::timestamptz IS NULL OR sub.current_period_ends_at <= sqlc.narg(expires_before)::timestamptz)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (sub.created_at, sub.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at DESC, sub.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: GetLifecycleSubscriptionByCustomerAndTierGroup :one
SELECT sub.* FROM billing.subscriptions sub
JOIN billing.products prod ON prod.id = sub.product_id
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
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.product_id = $2
  AND sub.status = 'unverified'
  AND sub.deleted_at IS NULL
ORDER BY sub.current_period_ends_at DESC NULLS FIRST
LIMIT 1;

-- name: GetUnknownSubscriptionByCustomerAndTierGroup :one
SELECT sub.* FROM billing.subscriptions sub
JOIN billing.products prod ON prod.id = sub.product_id
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND prod.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.status = 'unverified'
  AND prod.tier_group = $2
  AND sub.deleted_at IS NULL
ORDER BY sub.current_period_ends_at DESC NULLS FIRST
LIMIT 1;

-- name: ListSubscriptionsByPaymentMethodIDs :many
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.payment_method_id = ANY(sqlc.arg(payment_method_ids)::uuid[])
  AND sub.deleted_at IS NULL;

-- name: MarkCanceledSubscriptionsSuperseded :execrows
-- Preserve canceled subscriptions for refund/chargeback correlation while
-- stamping them superseded by the new activation (gateway_response patch).
UPDATE billing.subscriptions
SET gateway_response = CASE WHEN jsonb_typeof(gateway_response) = 'object'
        THEN gateway_response || jsonb_build_object('superseded_at', current_timestamp, 'superseded_by_subscription_id', sqlc.narg(superseded_by)::text)
        ELSE jsonb_build_object('previous_gateway_response', gateway_response, 'superseded_at', current_timestamp, 'superseded_by_subscription_id', sqlc.narg(superseded_by)::text)
    END,
    updated_at = current_timestamp
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = $1
  AND product_id = $2
  AND status = 'canceled'
  AND (sqlc.narg(exclude_id)::uuid IS NULL OR id != sqlc.narg(exclude_id)::uuid)
  AND deleted_at IS NULL;

-- CROSS-MERCHANT: merchants with due dunning work on the named rails. Three
-- legs, each served by its own partial index so a pass reads only due rows:
-- provider-billed NMI retries, expired awaiting_method grace, and engine
-- collections due. Ids only; due rows and charges run per merchant.
-- name: ListDueDunningMerchants :many
SELECT d.merchant_id
FROM (
    SELECT s.merchant_id, s.next_retry_at AS due_at
      FROM billing.subscriptions s
     WHERE s.status = 'past_due' AND s.next_retry_at IS NOT NULL
       AND s.next_retry_at <= sqlc.arg(now)::timestamptz
       AND s.rail = 'nmi' AND s.rail = ANY(sqlc.arg(rails)::text[])
       AND s.collection_policy <> 'engine'
       AND s.deleted_at IS NULL
    UNION ALL
    SELECT s.merchant_id, s.grace_ends_at
      FROM billing.subscriptions s
     WHERE s.grace_ends_at IS NOT NULL
       AND s.grace_ends_at <= sqlc.arg(now)::timestamptz
       AND s.status = 'awaiting_method'
       AND s.rail = ANY(sqlc.arg(rails)::text[])
       AND ((sqlc.arg(include_engine)::boolean AND s.collection_policy = 'engine')
            OR (s.collection_policy = 'nmi_schedule' AND s.rail = 'nmi'))
       AND s.deleted_at IS NULL
    UNION ALL
    SELECT s.merchant_id, CASE WHEN s.status = 'active' THEN s.current_period_ends_at ELSE s.next_retry_at END
      FROM billing.subscriptions s
     WHERE sqlc.arg(include_engine)::boolean
       AND s.collection_policy = 'engine' AND s.status IN ('active', 'past_due') AND s.deleted_at IS NULL
       AND s.current_period_ends_at <= sqlc.arg(now)::timestamptz
       AND (s.status = 'active' OR s.next_retry_at <= sqlc.arg(now)::timestamptz)
       AND s.rail = ANY(sqlc.arg(rails)::text[])
       AND NOT EXISTS (
             SELECT 1 FROM billing.provider_intents i
              WHERE i.merchant_id = s.merchant_id AND i.subscription_id = s.id
                AND i.intent_type = 'subscription_collection'
                AND i.status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable'))
) d
GROUP BY d.merchant_id
ORDER BY MIN(d.due_at), d.merchant_id
LIMIT sqlc.arg(merchant_limit)::int;

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
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.rail = ANY(sqlc.arg(rails)::text[])
  AND ((sub.collection_policy <> 'engine' AND sub.rail='nmi' AND sub.status='past_due' AND sub.next_retry_at IS NOT NULL AND sub.next_retry_at <= sqlc.arg(now)::timestamptz)
       OR (sub.status='awaiting_method' AND sub.grace_ends_at <= sqlc.arg(now)::timestamptz
           AND ((sqlc.arg(include_engine)::boolean AND sub.collection_policy='engine') OR (sub.collection_policy='nmi_schedule' AND sub.rail='nmi')))
       OR (sqlc.arg(include_engine)::boolean AND sub.collection_policy='engine' AND sub.current_period_ends_at <= sqlc.arg(now)::timestamptz
           AND (sub.status='active' OR (sub.status='past_due' AND sub.next_retry_at <= sqlc.arg(now)::timestamptz))
           AND NOT EXISTS (SELECT 1 FROM billing.provider_intents i WHERE i.merchant_id=sub.merchant_id AND i.subscription_id=sub.id AND i.intent_type='subscription_collection' AND i.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable'))))
  AND sub.deleted_at IS NULL
ORDER BY CASE WHEN sub.status='awaiting_method' THEN sub.grace_ends_at WHEN sub.collection_policy='engine' AND sub.status='active' THEN sub.current_period_ends_at ELSE sub.next_retry_at END, sub.id
LIMIT sqlc.arg(row_limit)::int;

-- name: GetLatestResumableCanceledSubscription :one
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.customer_id = $1
  AND sub.status = 'canceled'
  AND (sub.current_period_ends_at IS NULL OR sub.current_period_ends_at > sqlc.arg(now)::timestamptz)
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at DESC
LIMIT 1;

-- #813: the plan-migration cohort — every subscription still billing (or
-- still being dunned) on the retired price. past_due is INCLUDED: a sub whose
-- dunning recovers would otherwise renew on the old plan and silently escape
-- the migration.
-- name: ListMigratableSubscriptionsByPriceID :many
SELECT * FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.price_id = sqlc.arg(price_id)::uuid
  AND sub.status IN ('active', 'past_due', 'awaiting_method')
  AND sub.deleted_at IS NULL
ORDER BY sub.created_at;

-- NMI deletion completion owns only this read-model marker. A full-row replay
-- can undo another command's price/card/period/quote while waiting for the lock.
-- name: ClearSubscriptionDeletionMarker :execrows
UPDATE billing.subscriptions
SET deletion_scheduled_at=NULL, updated_at=sqlc.arg(now)::timestamptz
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
  AND psp_id=sqlc.arg(psp_id)::uuid
  AND rail_subscription_id=sqlc.arg(rail_subscription_id)::text
  AND deleted_at IS NULL
  AND deletion_scheduled_at IS NOT NULL;

-- name: GetInitialMembershipForUpdate :one
-- Accepted completion must see tombstones so it never resurrects a membership.
SELECT * FROM billing.subscriptions
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
FOR UPDATE;

-- name: GetInitialMembershipForArchive :one
SELECT * FROM billing.subscriptions
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid;

-- name: ListSubscriptionsToWake :many
-- The delinquent memberships OpenRails collects that a replaced card retries
-- at the next due pass, and those awaiting a card
-- (subscriptions.WakeForReplacedMethod).
SELECT id FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND collection_policy IN ('engine', 'nmi_schedule')
  AND (status = 'awaiting_method' OR (status = 'past_due' AND (next_retry_at IS NULL OR next_retry_at > sqlc.arg(now)::timestamptz)))
  AND deleted_at IS NULL
ORDER BY id;

-- name: ListLiveSubscriptionsOnMethod :many
-- #1115: the memberships a stored card pays for.
SELECT id FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND status IN ('active', 'past_due', 'awaiting_method', 'unverified')
  AND deleted_at IS NULL
ORDER BY id;

-- Declared import: seed-time forensics land on the new row only.
-- name: StampImportedSubscriptionEvidence :exec
UPDATE billing.subscriptions
SET gateway_response = COALESCE(sqlc.narg(gateway_response)::jsonb, gateway_response),
    retry_attempts = GREATEST(retry_attempts, sqlc.arg(retry_attempts)::bigint),
    last_retry_at = COALESCE(sqlc.narg(last_retry_at)::timestamptz, last_retry_at)
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid AND deleted_at IS NULL;

-- name: LinkImportedSubscriptionPaymentMethod :exec
UPDATE billing.subscriptions SET payment_method_id = sqlc.arg(payment_method_id)::uuid
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid AND payment_method_id IS NULL
  AND deleted_at IS NULL;

-- name: ScheduleImportedSubscriptionDeletion :exec
UPDATE billing.subscriptions
SET deletion_scheduled_at = sqlc.arg(at)::timestamptz, updated_at = sqlc.arg(at)::timestamptz
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid AND deletion_scheduled_at IS NULL
  AND deleted_at IS NULL;
