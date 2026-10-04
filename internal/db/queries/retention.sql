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
