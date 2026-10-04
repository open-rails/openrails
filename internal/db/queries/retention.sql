-- or#837 retention sweep: due-work discovery + the durable resume cursor.

-- CROSS-MERCHANT: merchants holding at least one row past a retention cutoff.
-- Ids only; every delete runs per merchant in bounded batches. Capped and
-- cursored: one pass is bounded work and the next resumes after the last
-- merchant handled.
-- name: ListRetentionWorkMerchants :many
SELECT q.mid AS merchant_id
FROM (
    (SELECT DISTINCT cs.merchant_id AS mid
       FROM billing.checkout_attempts cs
      WHERE (sqlc.narg(after)::uuid IS NULL OR cs.merchant_id > sqlc.narg(after)::uuid)
        AND cs.expires_at IS NOT NULL AND cs.expires_at < sqlc.arg(now)::timestamptz
        AND cs.deleted_at IS NULL
        AND cs.status IN ('created', 'requires_action')
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT nq.merchant_id AS mid
       FROM billing.notifications nq
      WHERE (sqlc.narg(after)::uuid IS NULL OR nq.merchant_id > sqlc.narg(after)::uuid)
        -- The GREATEST bound is implied by both arms and lets created_at drive the index.
        AND nq.created_at < GREATEST(sqlc.arg(notification_cutoff)::timestamptz, sqlc.arg(notification_seen_cutoff)::timestamptz)
        AND (nq.created_at < sqlc.arg(notification_cutoff)::timestamptz
             OR (nq.read_at IS NOT NULL AND nq.created_at < sqlc.arg(notification_seen_cutoff)::timestamptz))
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT we.merchant_id AS mid
       FROM billing.webhook_events we
      WHERE (sqlc.narg(after)::uuid IS NULL OR we.merchant_id > sqlc.narg(after)::uuid)
        AND we.completed_at < sqlc.arg(webhook_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT pse.merchant_id AS mid
       FROM billing.host_outbox pse
      WHERE (sqlc.narg(after)::uuid IS NULL OR pse.merchant_id > sqlc.narg(after)::uuid)
        AND pse.event_type = 'payment.settled' AND pse.delivered_at IS NOT NULL
        AND pse.delivered_at < sqlc.arg(settlement_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT hle.merchant_id AS mid
       FROM billing.host_outbox hle
      WHERE (sqlc.narg(after)::uuid IS NULL OR hle.merchant_id > sqlc.narg(after)::uuid)
        AND hle.event_type <> 'payment.settled' AND hle.delivered_at IS NOT NULL
        AND hle.delivered_at < sqlc.arg(lifecycle_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT pa.merchant_id AS mid
       FROM billing.payment_attempts pa
      WHERE (sqlc.narg(after)::uuid IS NULL OR pa.merchant_id > sqlc.narg(after)::uuid)
        AND pa.attempted_at < sqlc.arg(attempt_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT rc.merchant_id AS mid
       FROM billing.rebill_cycles rc
      WHERE (sqlc.narg(after)::uuid IS NULL OR rc.merchant_id > sqlc.narg(after)::uuid)
        AND rc.due_at < sqlc.arg(attempt_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT nh.merchant_id AS mid
       FROM billing.nmi_history_months nh
      WHERE (sqlc.narg(after)::uuid IS NULL OR nh.merchant_id > sqlc.narg(after)::uuid)
        AND nh.month < sqlc.arg(attempt_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT st.merchant_id AS mid
       FROM billing.subscription_status_transitions st
      WHERE (sqlc.narg(after)::uuid IS NULL OR st.merchant_id > sqlc.narg(after)::uuid)
        AND st.occurred_at < sqlc.arg(transition_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT ca.merchant_id AS mid
       FROM billing.checkout_attempts ca
      WHERE (sqlc.narg(after)::uuid IS NULL OR ca.merchant_id > sqlc.narg(after)::uuid)
        AND ca.status = 'expired' AND ca.payment_id IS NULL AND ca.subscription_id IS NULL AND ca.transaction_id IS NULL
        AND ca.expires_at < sqlc.arg(checkout_attempt_cutoff)::timestamptz
        AND NOT EXISTS (SELECT 1 FROM billing.solana_pay_references r WHERE r.merchant_id = ca.merchant_id AND r.checkout_attempt_id = ca.id)
        AND NOT EXISTS (SELECT 1 FROM billing.solana_pay_receipts rc WHERE rc.merchant_id = ca.merchant_id AND rc.checkout_attempt_id = ca.id)
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT rf.merchant_id AS mid
       FROM billing.reconciliation_findings rf
      WHERE (sqlc.narg(after)::uuid IS NULL OR rf.merchant_id > sqlc.narg(after)::uuid)
        AND rf.resolved_at IS NOT NULL
        AND GREATEST(rf.resolved_at, rf.last_seen_at) < sqlc.arg(finding_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    -- A run a finding still names is not due: counting it would revisit its
    -- merchant every pass to delete nothing.
    (SELECT DISTINCT mr.merchant_id AS mid
       FROM billing.maintenance_runs mr
      WHERE (sqlc.narg(after)::uuid IS NULL OR mr.merchant_id > sqlc.narg(after)::uuid)
        AND mr.kind = 'reconciliation' AND mr.started_at < sqlc.arg(run_cutoff)::timestamptz
        AND NOT EXISTS (SELECT 1 FROM billing.reconciliation_findings f WHERE f.merchant_id = mr.merchant_id AND f.first_seen_run = mr.id)
        AND NOT EXISTS (SELECT 1 FROM billing.reconciliation_findings f WHERE f.merchant_id = mr.merchant_id AND f.last_seen_run = mr.id)
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
) q
ORDER BY q.mid
LIMIT sqlc.arg(merchant_limit)::int;

-- name: GetSweepCursor :one
SELECT cursor_merchant_id, cursor_version FROM billing.worker_state
WHERE worker_kind = sqlc.arg(worker_kind)::text;

-- NULL parks the cursor at the start of the ring: the pass drained its queue.
-- The ring wraps inside a pass, so the next cursor is not ordered against the
-- previous one; monotonicity is a compare-and-swap on the opaque cursor_version
-- the pass read, bumped by every applied save. A pass finishing after a newer
-- pass already moved the cursor affects 0 rows and keeps the newer position.
-- name: SaveSweepCursor :execrows
INSERT INTO billing.worker_state (worker_kind, cursor_merchant_id, cursor_version)
VALUES (sqlc.arg(worker_kind)::text, sqlc.narg(cursor_merchant_id)::uuid, 1)
ON CONFLICT (worker_kind) DO UPDATE
    SET cursor_merchant_id = EXCLUDED.cursor_merchant_id,
        cursor_version = billing.worker_state.cursor_version + 1
    WHERE billing.worker_state.cursor_version
          = sqlc.arg(expected_cursor_version)::bigint;

-- Calendar-driven partition maintenance: neither statement reads a row.
-- name: EnsureMonthPartitions :one
SELECT billing.ensure_month_partitions(sqlc.arg(table_name)::name, sqlc.arg(from_at)::timestamptz, sqlc.arg(through_at)::timestamptz)::int AS created;

-- name: DropMonthPartitions :one
SELECT billing.drop_month_partitions(sqlc.arg(table_name)::name, sqlc.arg(before)::timestamptz)::int AS dropped;

-- name: ListMonthPartitions :many
SELECT m.partition::text AS partition, m.range_from::timestamptz AS range_from, m.range_to::timestamptz AS range_to
FROM billing.month_partitions(sqlc.arg(table_name)::name) m;

-- Row retention. Each statement deletes one bounded batch, oldest first, of
-- one merchant's rows past their period; the cleanup worker loops.

-- Tables that refuse ad-hoc deletes take them only from a transaction that
-- named the table here, and only for rows past the period their trigger
-- declares (billing.guard_retention_delete). Their sweeps count the period on
-- the database clock, the one the trigger reads.
-- name: DeclareRetentionSweep :exec
SELECT set_config('openrails.retention', sqlc.arg(table_name)::text, true);

-- name: DeleteSubscriptionTransitionsPastRetention :execrows
DELETE FROM billing.subscription_status_transitions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id IN (
    SELECT st.id FROM billing.subscription_status_transitions st
    WHERE st.merchant_id = sqlc.arg(merchant_id)::uuid
      AND st.occurred_at < now() - make_interval(days => sqlc.arg(retention_days)::int)
    ORDER BY st.occurred_at
    LIMIT sqlc.arg(row_limit)::int
);

-- name: DeleteReconciliationRunsPastRetention :execrows
DELETE FROM billing.maintenance_runs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id IN (
    SELECT mr.id FROM billing.maintenance_runs mr
    WHERE mr.merchant_id = sqlc.arg(merchant_id)::uuid
      AND mr.kind = 'reconciliation'
      AND mr.started_at < now() - make_interval(days => sqlc.arg(retention_days)::int)
      AND NOT EXISTS (SELECT 1 FROM billing.reconciliation_findings f WHERE f.merchant_id = mr.merchant_id AND f.first_seen_run = mr.id)
      AND NOT EXISTS (SELECT 1 FROM billing.reconciliation_findings f WHERE f.merchant_id = mr.merchant_id AND f.last_seen_run = mr.id)
    ORDER BY mr.started_at
    LIMIT sqlc.arg(row_limit)::int
);

-- A finding still being seen is kept however long ago it was resolved: an
-- ignored drift that persists must not come back as new.
-- name: DeleteResolvedFindingsBefore :execrows
DELETE FROM billing.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id IN (
    SELECT rf.id FROM billing.reconciliation_findings rf
    WHERE rf.merchant_id = sqlc.arg(merchant_id)::uuid
      AND rf.resolved_at IS NOT NULL
      AND GREATEST(rf.resolved_at, rf.last_seen_at) < sqlc.arg(cutoff)::timestamptz
    ORDER BY GREATEST(rf.resolved_at, rf.last_seen_at)
    LIMIT sqlc.arg(row_limit)::int
);

-- An attempt that expired without reaching a provider: no payment,
-- subscription or provider transaction, and no Solana Pay reference or receipt
-- names it. Its checkout session, if one still exists, goes with it.
-- name: DeleteAbandonedCheckoutAttemptsBefore :execrows
DELETE FROM billing.checkout_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id IN (
    SELECT ca.id FROM billing.checkout_attempts ca
    WHERE ca.merchant_id = sqlc.arg(merchant_id)::uuid
      AND ca.status = 'expired' AND ca.payment_id IS NULL AND ca.subscription_id IS NULL AND ca.transaction_id IS NULL
      AND ca.expires_at < sqlc.arg(cutoff)::timestamptz
      AND NOT EXISTS (SELECT 1 FROM billing.solana_pay_references r WHERE r.merchant_id = ca.merchant_id AND r.checkout_attempt_id = ca.id)
      AND NOT EXISTS (SELECT 1 FROM billing.solana_pay_receipts rc WHERE rc.merchant_id = ca.merchant_id AND rc.checkout_attempt_id = ca.id)
    ORDER BY ca.expires_at
    LIMIT sqlc.arg(row_limit)::int
);
