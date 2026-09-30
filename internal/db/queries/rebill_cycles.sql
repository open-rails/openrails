-- name: UpsertRebillCycle :one
-- #1111: the cycle for a subscription's period that came due at due_at.
INSERT INTO openrails.rebill_cycles (id, merchant_id, subscription_id, customer_id, psp_id, rail, owner, due_at, amount, currency)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(customer_id)::uuid,
    sqlc.arg(psp_id)::uuid, sqlc.arg(rail)::text, sqlc.arg(owner)::text, sqlc.arg(due_at)::timestamptz,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text)
ON CONFLICT (merchant_id, subscription_id, due_at) DO UPDATE SET due_at = EXCLUDED.due_at
RETURNING id;

-- name: ListOverdueRebillMerchants :many
SELECT merchant_id FROM openrails.overdue_rebill_merchant_ids(
    sqlc.arg(engine_cutoff)::timestamptz, sqlc.arg(nmi_cutoff)::timestamptz, sqlc.arg(merchant_limit)::int);

-- name: ListOverdueRebills :many
-- #1112: auto-renewing subscriptions whose period ended before its owner's
-- deadline with neither an attempt nor a recorded miss for that cycle.
SELECT sub.* FROM openrails.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid
  AND sub.status IN ('active', 'unverified', 'awaiting_method') AND sub.deleted_at IS NULL
  AND ((sub.collection_policy = 'engine' AND sub.current_period_ends_at <= sqlc.arg(engine_cutoff)::timestamptz)
       OR (sub.collection_policy = 'nmi_schedule' AND sub.current_period_ends_at <= sqlc.arg(nmi_cutoff)::timestamptz))
  AND NOT EXISTS (
      SELECT 1 FROM openrails.rebill_cycles c
       WHERE c.merchant_id = sub.merchant_id AND c.subscription_id = sub.id AND c.due_at = sub.current_period_ends_at
         AND (c.missed_at IS NOT NULL OR EXISTS (SELECT 1 FROM openrails.payment_attempts a WHERE a.merchant_id = c.merchant_id AND a.cycle_id = c.id)))
ORDER BY sub.current_period_ends_at, sub.id
LIMIT sqlc.arg(row_limit)::int;

-- name: CycleHasAttempt :one
SELECT EXISTS (
    SELECT 1 FROM openrails.rebill_cycles c JOIN openrails.payment_attempts a ON a.merchant_id = c.merchant_id AND a.cycle_id = c.id
     WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid AND c.subscription_id = sqlc.arg(subscription_id)::uuid AND c.due_at = sqlc.arg(due_at)::timestamptz
)::boolean AS attempted;

-- name: MarkRebillCycleMissed :execrows
UPDATE openrails.rebill_cycles SET missed_at = sqlc.arg(missed_at)::timestamptz, miss_reason = sqlc.arg(miss_reason)::text
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND missed_at IS NULL;

-- name: CountOpenSubscriptionCollections :one
-- An engine renewal still being attempted decides its cycle itself.
SELECT count(*) FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND intent_type = 'subscription_collection' AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable');

-- name: ListRebillCycles :many
-- #1116: the merchant's rebill cycles as rebill_cycle_facts derives them,
-- latest due first. outcome filters on collected, lost (closed by now
-- without a collection) or open.
SELECT sqlc.embed(cf), count(*) OVER () AS total
FROM openrails.rebill_cycle_facts cf
WHERE cf.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(owner)::text IS NULL OR cf.owner = sqlc.narg(owner)::text)
  AND (sqlc.narg(first_outcome)::text IS NULL OR cf.first_outcome = sqlc.narg(first_outcome)::text)
  AND (sqlc.narg(miss_reason)::text IS NULL OR cf.miss_reason = sqlc.narg(miss_reason)::text)
  AND (sqlc.narg(psp_id)::uuid IS NULL OR cf.psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR cf.subscription_id = sqlc.narg(subscription_id)::uuid)
  AND (sqlc.narg(due_since)::timestamptz IS NULL OR cf.due_at >= sqlc.narg(due_since)::timestamptz)
  AND (sqlc.narg(due_until)::timestamptz IS NULL OR cf.due_at < sqlc.narg(due_until)::timestamptz)
  AND CASE sqlc.narg(outcome)::text
        WHEN 'collected' THEN cf.won_at IS NOT NULL
        WHEN 'lost' THEN cf.won_at IS NULL AND cf.closed_at <= sqlc.arg(now)::timestamptz
        WHEN 'open' THEN cf.won_at IS NULL AND cf.closed_at > sqlc.arg(now)::timestamptz
        ELSE true END
ORDER BY cf.due_at DESC, cf.id DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: GetRebillCycle :one
SELECT * FROM openrails.rebill_cycle_facts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;
