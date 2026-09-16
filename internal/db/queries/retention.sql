-- or#837 retention sweep: due-work discovery + the durable resume cursor.

-- CROSS-MERCHANT: merchants holding at least one row past a retention cutoff,
-- through migration 0056's SECURITY DEFINER work queue. Ids only — every delete
-- runs per-merchant under RunInMerchantScope. Capped and cursored: one pass is
-- bounded work and the next resumes at the merchant after the last one handled.
-- name: ListRetentionWorkMerchants :many
SELECT merchant_id FROM openrails.retention_work_merchant_ids(
    sqlc.arg(now)::timestamptz,
    sqlc.arg(notification_cutoff)::timestamptz,
    sqlc.arg(notification_seen_cutoff)::timestamptz,
    sqlc.arg(webhook_cutoff)::timestamptz,
    sqlc.arg(settlement_cutoff)::timestamptz,
    sqlc.arg(lifecycle_cutoff)::timestamptz,
    sqlc.narg(after)::uuid,
    sqlc.arg(merchant_limit)::int);

-- name: GetSweepCursor :one
SELECT cursor_merchant_id, cursor_updated_at FROM openrails.worker_state
WHERE worker_kind = sqlc.arg(worker_kind)::text;

-- NULL parks the cursor at the start of the ring: the pass drained its queue.
-- The ring wraps inside a pass, so the next cursor is not ordered against the
-- previous one; monotonicity is a compare-and-swap on the cursor version the
-- pass read (cursor_updated_at). A pass finishing after a newer pass already
-- moved the cursor affects 0 rows and keeps the newer position.
-- name: SaveSweepCursor :execrows
INSERT INTO openrails.worker_state (worker_kind, cursor_merchant_id, cursor_updated_at)
VALUES (sqlc.arg(worker_kind)::text, sqlc.narg(cursor_merchant_id)::uuid, clock_timestamp())
ON CONFLICT (worker_kind) DO UPDATE
    SET cursor_merchant_id = EXCLUDED.cursor_merchant_id,
        cursor_updated_at = EXCLUDED.cursor_updated_at
    WHERE openrails.worker_state.cursor_updated_at
          IS NOT DISTINCT FROM sqlc.narg(expected_cursor_updated_at)::timestamptz;
