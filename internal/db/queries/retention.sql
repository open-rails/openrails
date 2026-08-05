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
SELECT cursor_merchant_id FROM openrails.worker_sweep_cursors
WHERE worker_kind = sqlc.arg(worker_kind)::text;

-- NULL parks the cursor at the start of the ring: the pass drained its queue.
-- name: SaveSweepCursor :exec
INSERT INTO openrails.worker_sweep_cursors (worker_kind, cursor_merchant_id, updated_at)
VALUES (sqlc.arg(worker_kind)::text, sqlc.narg(cursor_merchant_id)::uuid, now())
ON CONFLICT (worker_kind) DO UPDATE
    SET cursor_merchant_id = EXCLUDED.cursor_merchant_id,
        updated_at = EXCLUDED.updated_at;
