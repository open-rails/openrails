-- openrails.subscription_reprices (#773): per-subscription scheduled/applied/
-- canceled price move.

-- name: CreateSubscriptionReprice :one
INSERT INTO openrails.subscription_reprices (
    merchant_id, subscription_id, from_price_id, to_price_id, effective_at, reprice_batch_id, acknowledged_short_notice, kind
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(from_price_id)::uuid,
    sqlc.arg(to_price_id)::uuid, sqlc.arg(effective_at)::timestamptz, sqlc.narg(reprice_batch_id)::uuid,
    sqlc.arg(acknowledged_short_notice)::bool, sqlc.arg(kind)::text
)
RETURNING *;

-- #813: a per-subscription ledger row for a cohort member the engine could
-- NOT auto-schedule (rail requires user action / missing rail config / rail
-- push failure). Terminal at insert.
-- name: CreateBlockedSubscriptionReprice :one
INSERT INTO openrails.subscription_reprices (
    merchant_id, subscription_id, from_price_id, to_price_id, effective_at, reprice_batch_id, kind, status, blocked_reason
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(from_price_id)::uuid,
    sqlc.arg(to_price_id)::uuid, sqlc.arg(effective_at)::timestamptz, sqlc.narg(reprice_batch_id)::uuid,
    sqlc.arg(kind)::text, 'blocked', sqlc.arg(blocked_reason)::text
)
RETURNING *;

-- name: GetSubscriptionRepriceByID :one
SELECT * FROM openrails.subscription_reprices WHERE id = $1;

-- The subscription's current scheduled reprice, if any (at most one by
-- uq_subscription_reprices_one_scheduled) — used both to refuse a second
-- schedule and, at the renewal boundary, to check whether it is DUE
-- (effective_at <= now, checked in Go).
-- name: GetScheduledRepriceForSubscription :one
SELECT * FROM openrails.subscription_reprices
WHERE subscription_id = sqlc.arg(subscription_id)::uuid AND status = 'scheduled'
LIMIT 1;

-- name: ListSubscriptionReprices :many
SELECT * FROM openrails.subscription_reprices
WHERE (sqlc.narg(subscription_id)::uuid IS NULL OR subscription_id = sqlc.narg(subscription_id)::uuid)
  AND (sqlc.narg(reprice_batch_id)::uuid IS NULL OR reprice_batch_id = sqlc.narg(reprice_batch_id)::uuid)
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: CancelSubscriptionReprice :execrows
UPDATE openrails.subscription_reprices SET
    status = 'canceled',
    canceled_at = now()
WHERE id = sqlc.arg(id) AND status = 'scheduled';

-- name: ApplySubscriptionReprice :execrows
UPDATE openrails.subscription_reprices SET
    status = 'applied',
    applied_at = now()
WHERE id = sqlc.arg(id) AND status = 'scheduled';

-- #813: converge hook — when an OBSERVED rail (stripe) reports the
-- subscription now carries the target price, the matching scheduled row is
-- marked applied inside the converge transaction. Idempotent by predicate.
-- name: ApplyScheduledRepriceForSubscriptionPrice :execrows
UPDATE openrails.subscription_reprices SET
    status = 'applied',
    applied_at = now()
WHERE subscription_id = sqlc.arg(subscription_id)::uuid
  AND to_price_id = sqlc.arg(to_price_id)::uuid
  AND status = 'scheduled';

-- #813: a scheduled row whose rail push failed after creation — terminal,
-- with the reason preserved for the batch ledger.
-- name: BlockSubscriptionReprice :execrows
UPDATE openrails.subscription_reprices SET
    status = 'blocked',
    blocked_reason = sqlc.arg(blocked_reason)::text
WHERE id = sqlc.arg(id) AND status = 'scheduled';

-- #816: the re-driver's cross-merchant enumeration. Only push-failure blocks
-- are re-drivable (the prefix covers the deferred-window refusals and the NMI
-- push-verified-then-crash window); classification blocks
-- (rail_requires_user_action, interval/cent mismatches, missing rail config
-- at classify time) never carried a push attempt and stay terminal.
-- name: ListRedrivableBlockedPlanChangeReprices :many
SELECT * FROM openrails.subscription_reprices
WHERE kind = 'plan_change'
  AND status = 'blocked'
  AND blocked_reason LIKE 'rail_push_failed:%'
ORDER BY created_at
LIMIT sqlc.arg(batch_size)::int;

-- #816: the re-driver's un-block — the exact inverse of
-- BlockSubscriptionReprice, status-predicated so a concurrent transition wins
-- cleanly. uq_subscription_reprices_one_scheduled makes this fail (unique
-- violation) if the subscription acquired another scheduled row meanwhile —
-- callers treat that as a skip.
-- name: UnblockSubscriptionReprice :execrows
UPDATE openrails.subscription_reprices SET
    status = 'scheduled',
    blocked_reason = ''
WHERE id = sqlc.arg(id) AND status = 'blocked';

-- #816: batch-header re-sync source of truth. Rows exist only for
-- scheduled/blocked cohort members (skips are header-only), and the header's
-- "scheduled" has always counted every auto-migratable row regardless of how
-- far it progressed — so non-blocked = scheduled|applied|canceled.
-- name: CountPlanMigrationBatchRows :one
SELECT
    count(*) FILTER (WHERE status = 'blocked')  AS blocked,
    count(*) FILTER (WHERE status <> 'blocked') AS scheduled
FROM openrails.subscription_reprices
WHERE reprice_batch_id = sqlc.arg(batch_id)::uuid;
