-- name: UpsertRebillCycle :one
-- The cycle for a subscription's period that came due at due_at.
INSERT INTO billing.rebill_cycles (id, merchant_id, subscription_id, customer_id, psp_id, rail, owner, due_at, amount, currency, quantity)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(customer_id)::uuid,
    sqlc.arg(psp_id)::uuid, sqlc.arg(rail)::text, sqlc.arg(owner)::text, sqlc.arg(due_at)::timestamptz,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.narg(quantity)::int)
ON CONFLICT (merchant_id, subscription_id, due_at) DO UPDATE SET due_at = EXCLUDED.due_at
RETURNING id;

-- name: ListOverdueRebillMerchants :many
-- CROSS-MERCHANT: merchants holding an NMI schedule whose period ended before
-- the cutoff with neither an attempt nor a recorded miss for that cycle, and
-- no failed probe waiting out its backoff. Ids in order after a cursor, so a
-- pass reaches every merchant.
SELECT DISTINCT s.merchant_id
FROM billing.subscriptions s
WHERE s.status IN ('active', 'unverified', 'awaiting_method') AND s.deleted_at IS NULL
  AND s.merchant_id > sqlc.arg(after_merchant_id)::uuid
  AND s.collection_policy = 'nmi_schedule' AND s.current_period_ends_at <= sqlc.arg(cutoff)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM billing.rebill_cycles c
       WHERE c.merchant_id = s.merchant_id AND c.subscription_id = s.id AND c.due_at = s.current_period_ends_at
         AND (c.missed_at IS NOT NULL OR c.next_probe_at > sqlc.arg(now)::timestamptz))
  AND NOT EXISTS (
      SELECT 1 FROM billing.rebill_cycles c
        JOIN billing.payment_attempts a ON a.merchant_id = c.merchant_id AND a.cycle_id = c.id
       WHERE c.merchant_id = s.merchant_id AND c.subscription_id = s.id AND c.due_at = s.current_period_ends_at)
ORDER BY s.merchant_id
LIMIT sqlc.arg(merchant_limit)::int;

-- name: ListOverdueRebills :many
-- NMI schedules whose period ended before the cutoff with neither an attempt
-- nor a recorded miss for that cycle, and no failed probe waiting out its
-- backoff: a page after the (period end, id) cursor.
SELECT sub.* FROM billing.subscriptions sub
WHERE sub.merchant_id = sqlc.arg(merchant_id)::uuid
  AND sub.status IN ('active', 'unverified', 'awaiting_method') AND sub.deleted_at IS NULL
  AND sub.collection_policy = 'nmi_schedule' AND sub.current_period_ends_at <= sqlc.arg(cutoff)::timestamptz
  AND (sub.current_period_ends_at, sub.id) > (sqlc.arg(after_due_at)::timestamptz, sqlc.arg(after_id)::uuid)
  AND NOT EXISTS (
      SELECT 1 FROM billing.rebill_cycles c
       WHERE c.merchant_id = sub.merchant_id AND c.subscription_id = sub.id AND c.due_at = sub.current_period_ends_at
         AND (c.missed_at IS NOT NULL OR c.next_probe_at > sqlc.arg(now)::timestamptz))
  AND NOT EXISTS (
      SELECT 1 FROM billing.rebill_cycles c
        JOIN billing.payment_attempts a ON a.merchant_id = c.merchant_id AND a.cycle_id = c.id
       WHERE c.merchant_id = sub.merchant_id AND c.subscription_id = sub.id AND c.due_at = sub.current_period_ends_at)
ORDER BY sub.current_period_ends_at, sub.id
LIMIT sqlc.arg(row_limit)::int;

-- name: RecordRebillProbeFailure :one
-- A failed look at an overdue cycle: it is looked at again after a backoff
-- that doubles from 15 minutes, at most a day.
INSERT INTO billing.rebill_cycles (id, merchant_id, subscription_id, customer_id, psp_id, rail, owner, due_at, amount, currency, quantity, probe_failed_at, next_probe_at)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(customer_id)::uuid,
    sqlc.arg(psp_id)::uuid, sqlc.arg(rail)::text, sqlc.arg(owner)::text, sqlc.arg(due_at)::timestamptz,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.narg(quantity)::int,
    sqlc.arg(failed_at)::timestamptz, sqlc.arg(failed_at)::timestamptz + interval '15 minutes')
ON CONFLICT (merchant_id, subscription_id, due_at) DO UPDATE SET
    probe_failed_at = COALESCE(billing.rebill_cycles.probe_failed_at, EXCLUDED.probe_failed_at),
    next_probe_at = EXCLUDED.probe_failed_at + LEAST(interval '24 hours', GREATEST(interval '15 minutes',
        EXCLUDED.probe_failed_at - COALESCE(billing.rebill_cycles.probe_failed_at, EXCLUDED.probe_failed_at)))
RETURNING next_probe_at;

-- name: CycleHasAttempt :one
SELECT EXISTS (
    SELECT 1 FROM billing.rebill_cycles c JOIN billing.payment_attempts a ON a.merchant_id = c.merchant_id AND a.cycle_id = c.id
     WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid AND c.subscription_id = sqlc.arg(subscription_id)::uuid AND c.due_at = sqlc.arg(due_at)::timestamptz
)::boolean AS attempted;

-- name: MarkRebillCycleMissed :execrows
UPDATE billing.rebill_cycles SET missed_at = sqlc.arg(missed_at)::timestamptz, miss_reason = sqlc.arg(miss_reason)::text,
    probe_failed_at = NULL, next_probe_at = NULL
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND missed_at IS NULL;

-- name: ListRebillCycles :many
-- The merchant's rebill cycles, latest due first, each with its first attempt,
-- the attempt that collected it and when it closes: collected, the subscription
-- canceled, or 15 days past due (the dunning window is at most 14), whichever
-- is first. Outcome is collected, lost (closed by now uncollected) or open; a
-- text filter matches any of its values; a page continues after its cursor.
-- The metrics rebill_cycles family derives the same facts.
WITH cy AS (
    SELECT c.id, c.subscription_id, c.customer_id, c.psp_id, c.rail, c.owner, c.due_at, c.amount, c.currency, c.missed_at, c.miss_reason
      FROM billing.rebill_cycles c
     WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid
    UNION ALL
    -- An engine renewal nothing attempted by its deadline, with none in
    -- flight, is read as missed: a cycle row exists only once attempted.
    SELECT md5('renewal:' || s.id::text || ':' || extract(epoch FROM s.current_period_ends_at)::text)::uuid,
           s.id, s.customer_id, s.psp_id, s.rail, 'engine'::text, s.current_period_ends_at,
           pr.amount * COALESCE(s.quantity, 1)::bigint, pr.currency,
           s.current_period_ends_at + make_interval(secs => CASE WHEN s.rail = 'solana' THEN sqlc.arg(solana_deadline_seconds)::float8 ELSE sqlc.arg(engine_deadline_seconds)::float8 END),
           CASE WHEN s.status = 'awaiting_method' THEN 'method_unusable'
                WHEN EXISTS (SELECT 1 FROM billing.reconciliation_findings f
                              WHERE f.merchant_id = s.merchant_id AND f.finding_type = 'life.due_pass.refused' AND f.subject_key = s.id::text
                                AND f.status IN ('reconcile_required', 'requires_review')) THEN 'refused'
                ELSE 'not_attempted' END::text
      FROM billing.subscriptions s
      JOIN billing.prices pr ON pr.merchant_id = s.merchant_id AND pr.id = s.price_id
     WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
       AND s.collection_policy = 'engine' AND s.status IN ('active', 'awaiting_method') AND s.deleted_at IS NULL
       AND s.current_period_ends_at + make_interval(secs => CASE WHEN s.rail = 'solana' THEN sqlc.arg(solana_deadline_seconds)::float8 ELSE sqlc.arg(engine_deadline_seconds)::float8 END) <= sqlc.arg(now)::timestamptz
       AND NOT EXISTS (SELECT 1 FROM billing.rebill_cycles c
                        WHERE c.merchant_id = s.merchant_id AND c.subscription_id = s.id AND c.due_at = s.current_period_ends_at)
       AND NOT EXISTS (SELECT 1 FROM billing.provider_intents i
                        WHERE i.merchant_id = s.merchant_id AND i.subscription_id = s.id
                          AND i.intent_type IN ('subscription_collection', 'solana_pull')
                          AND i.status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable'))
), cf AS (
    SELECT c.id, c.subscription_id, c.customer_id, c.psp_id, c.rail, c.owner, c.due_at, c.amount, c.currency,
           c.missed_at, c.miss_reason, CASE WHEN w.id IS NOT NULL THEN w.attempted_at END AS won_at,
           CASE WHEN c.missed_at IS NOT NULL THEN 'missed'
                WHEN f.category IS NULL THEN 'pending'
                WHEN f.category = 'approved' THEN 'approved'
                WHEN f.category = 'system_error' THEN 'error'
                ELSE 'declined' END::text AS first_outcome,
           LEAST(w.attempted_at, CASE WHEN s.canceled_at IS NOT NULL THEN GREATEST(s.canceled_at, c.due_at) END,
                 c.due_at + interval '15 days')::timestamptz AS closed_at,
           CASE WHEN w.id IS NULL OR NOT (c.missed_at IS NOT NULL OR COALESCE(f.category <> 'approved', false)) THEN ''
                WHEN w.source = 'provider_schedule' THEN 'late_provider_charge'
                WHEN EXISTS (SELECT 1 FROM billing.payment_method_versions u
                              WHERE u.merchant_id = sqlc.arg(merchant_id)::uuid AND u.payment_method_id = w.payment_method_id
                                AND u.kind IN ('updated', 'brand_changed')
                                AND u.effective_at >= COALESCE(c.missed_at, f.attempted_at) AND u.effective_at <= w.attempted_at) THEN 'updated_card'
                WHEN w.kind = 'customer_retry' THEN 'customer_retry'
                ELSE 'dunning_retry' END::text AS recovered_by
      FROM cy c
      LEFT JOIN LATERAL (SELECT a.category, a.attempted_at FROM billing.payment_attempts a
                          WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.cycle_id = c.id ORDER BY a.attempted_at, a.id LIMIT 1) f ON true
      LEFT JOIN LATERAL (SELECT a.id, a.kind, a.source, a.attempted_at, a.payment_method_id FROM billing.payment_attempts a
                          WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.cycle_id = c.id AND a.category = 'approved'
                          ORDER BY a.attempted_at, a.id LIMIT 1) w ON true
      LEFT JOIN billing.subscriptions s ON s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.id = c.subscription_id AND s.deleted_at IS NULL
)
SELECT cf.*
FROM cf
WHERE (sqlc.narg(owners)::text[] IS NULL OR cf.owner = ANY(sqlc.narg(owners)::text[]))
  AND (sqlc.narg(first_outcomes)::text[] IS NULL OR cf.first_outcome = ANY(sqlc.narg(first_outcomes)::text[]))
  AND (sqlc.narg(miss_reasons)::text[] IS NULL OR cf.miss_reason = ANY(sqlc.narg(miss_reasons)::text[]))
  AND (sqlc.narg(psp_id)::uuid IS NULL OR cf.psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR cf.subscription_id = sqlc.narg(subscription_id)::uuid)
  AND (sqlc.narg(due_since)::timestamptz IS NULL OR cf.due_at >= sqlc.narg(due_since)::timestamptz)
  AND (sqlc.narg(due_until)::timestamptz IS NULL OR cf.due_at < sqlc.narg(due_until)::timestamptz)
  AND (sqlc.narg(outcomes)::text[] IS NULL OR CASE
        WHEN cf.won_at IS NOT NULL THEN 'collected'
        WHEN cf.closed_at <= sqlc.arg(now)::timestamptz THEN 'lost'
        ELSE 'open' END = ANY(sqlc.narg(outcomes)::text[]))
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (cf.due_at, cf.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY cf.due_at DESC, cf.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListRebillCyclesByIDs :many
-- The named cycles with ListRebillCycles' facts, latest due first.
WITH cy AS (
    SELECT c.id, c.subscription_id, c.customer_id, c.psp_id, c.rail, c.owner, c.due_at, c.amount, c.currency, c.missed_at, c.miss_reason
      FROM billing.rebill_cycles c
     WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid
    UNION ALL
    -- An engine renewal nothing attempted by its deadline, with none in
    -- flight, is read as missed: a cycle row exists only once attempted.
    SELECT md5('renewal:' || s.id::text || ':' || extract(epoch FROM s.current_period_ends_at)::text)::uuid,
           s.id, s.customer_id, s.psp_id, s.rail, 'engine'::text, s.current_period_ends_at,
           pr.amount * COALESCE(s.quantity, 1)::bigint, pr.currency,
           s.current_period_ends_at + make_interval(secs => CASE WHEN s.rail = 'solana' THEN sqlc.arg(solana_deadline_seconds)::float8 ELSE sqlc.arg(engine_deadline_seconds)::float8 END),
           CASE WHEN s.status = 'awaiting_method' THEN 'method_unusable'
                WHEN EXISTS (SELECT 1 FROM billing.reconciliation_findings f
                              WHERE f.merchant_id = s.merchant_id AND f.finding_type = 'life.due_pass.refused' AND f.subject_key = s.id::text
                                AND f.status IN ('reconcile_required', 'requires_review')) THEN 'refused'
                ELSE 'not_attempted' END::text
      FROM billing.subscriptions s
      JOIN billing.prices pr ON pr.merchant_id = s.merchant_id AND pr.id = s.price_id
     WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
       AND s.collection_policy = 'engine' AND s.status IN ('active', 'awaiting_method') AND s.deleted_at IS NULL
       AND s.current_period_ends_at + make_interval(secs => CASE WHEN s.rail = 'solana' THEN sqlc.arg(solana_deadline_seconds)::float8 ELSE sqlc.arg(engine_deadline_seconds)::float8 END) <= sqlc.arg(now)::timestamptz
       AND NOT EXISTS (SELECT 1 FROM billing.rebill_cycles c
                        WHERE c.merchant_id = s.merchant_id AND c.subscription_id = s.id AND c.due_at = s.current_period_ends_at)
       AND NOT EXISTS (SELECT 1 FROM billing.provider_intents i
                        WHERE i.merchant_id = s.merchant_id AND i.subscription_id = s.id
                          AND i.intent_type IN ('subscription_collection', 'solana_pull')
                          AND i.status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable'))
), cf AS (
    SELECT c.id, c.subscription_id, c.customer_id, c.psp_id, c.rail, c.owner, c.due_at, c.amount, c.currency,
           c.missed_at, c.miss_reason, CASE WHEN w.id IS NOT NULL THEN w.attempted_at END AS won_at,
           CASE WHEN c.missed_at IS NOT NULL THEN 'missed'
                WHEN f.category IS NULL THEN 'pending'
                WHEN f.category = 'approved' THEN 'approved'
                WHEN f.category = 'system_error' THEN 'error'
                ELSE 'declined' END::text AS first_outcome,
           LEAST(w.attempted_at, CASE WHEN s.canceled_at IS NOT NULL THEN GREATEST(s.canceled_at, c.due_at) END,
                 c.due_at + interval '15 days')::timestamptz AS closed_at,
           CASE WHEN w.id IS NULL OR NOT (c.missed_at IS NOT NULL OR COALESCE(f.category <> 'approved', false)) THEN ''
                WHEN w.source = 'provider_schedule' THEN 'late_provider_charge'
                WHEN EXISTS (SELECT 1 FROM billing.payment_method_versions u
                              WHERE u.merchant_id = sqlc.arg(merchant_id)::uuid AND u.payment_method_id = w.payment_method_id
                                AND u.kind IN ('updated', 'brand_changed')
                                AND u.effective_at >= COALESCE(c.missed_at, f.attempted_at) AND u.effective_at <= w.attempted_at) THEN 'updated_card'
                WHEN w.kind = 'customer_retry' THEN 'customer_retry'
                ELSE 'dunning_retry' END::text AS recovered_by
      FROM cy c
      LEFT JOIN LATERAL (SELECT a.category, a.attempted_at FROM billing.payment_attempts a
                          WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.cycle_id = c.id ORDER BY a.attempted_at, a.id LIMIT 1) f ON true
      LEFT JOIN LATERAL (SELECT a.id, a.kind, a.source, a.attempted_at, a.payment_method_id FROM billing.payment_attempts a
                          WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.cycle_id = c.id AND a.category = 'approved'
                          ORDER BY a.attempted_at, a.id LIMIT 1) w ON true
      LEFT JOIN billing.subscriptions s ON s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.id = c.subscription_id AND s.deleted_at IS NULL
     WHERE c.id = ANY(sqlc.arg(ids)::uuid[])
)
SELECT cf.*
FROM cf
ORDER BY cf.due_at DESC, cf.id DESC;

-- Cycles due before the retention cutoff whose attempts are all gone, batched
-- like the attempt purge that runs first.
-- name: DeleteRebillCyclesBefore :execrows
DELETE FROM billing.rebill_cycles
WHERE id IN (
    SELECT c.id FROM billing.rebill_cycles c
    WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid
      AND c.due_at < sqlc.arg(cutoff)::timestamptz
      AND NOT EXISTS (SELECT 1 FROM billing.payment_attempts a WHERE a.merchant_id = c.merchant_id AND a.cycle_id = c.id)
    LIMIT sqlc.arg(row_limit)::int
);

-- name: ListRenewalDeclines :many
-- Each named subscription's declines of the renewal due at due_at: how many,
-- and the latest one's reason. One row per subscription bounds row_limit.
SELECT c.subscription_id, d.declines, d.reason
FROM unnest(sqlc.arg(subscription_ids)::uuid[], sqlc.arg(due_ats)::timestamptz[]) AS k(subscription_id, due_at)
JOIN billing.rebill_cycles c ON c.merchant_id = sqlc.arg(merchant_id)::uuid AND c.subscription_id = k.subscription_id AND c.due_at = k.due_at
CROSS JOIN LATERAL (
    SELECT count(*)::int AS declines,
           (array_agg(pa.reason ORDER BY pa.attempted_at DESC, pa.id DESC))[1]::text AS reason
    FROM billing.payment_attempts pa
    WHERE pa.merchant_id = c.merchant_id AND pa.cycle_id = c.id AND pa.category <> 'approved'
) d
WHERE d.declines > 0
LIMIT sqlc.arg(row_limit)::int;
