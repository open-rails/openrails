-- name: GetEventLogStream :one
SELECT epoch,position,head_id,pruned_through,pruned_id FROM billing.event_log_streams
WHERE merchant_id=sqlc.arg(merchant_id)::uuid;

-- name: GetExpiredEventLogBoundary :one
SELECT position,id FROM billing.event_log
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND recorded_at<sqlc.arg(cutoff)::timestamptz
ORDER BY recorded_at DESC,position DESC LIMIT 1;

-- name: GetEventLogBoundary :one
SELECT id FROM billing.event_log
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND position=sqlc.arg(position)::bigint;

-- name: ListEventLog :many
SELECT position,id,source,event_type,version,occurred_at,recorded_at,subject_type,subject_id,data
FROM billing.event_log
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND position>sqlc.arg(after_position)::bigint
ORDER BY position LIMIT sqlc.arg(row_limit)::int;

-- name: ListEventLogByTypes :many
SELECT position,id,source,event_type,version,occurred_at,recorded_at,subject_type,subject_id,data
FROM billing.event_log
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND position>sqlc.arg(after_position)::bigint
AND event_type=ANY(sqlc.arg(event_types)::text[])
ORDER BY position LIMIT sqlc.arg(row_limit)::int;

-- CROSS-MERCHANT: bounded retention discovery returns only merchant IDs.
-- name: ListExpiredEventLogMerchants :many
SELECT merchant_id FROM billing.event_log
WHERE recorded_at<sqlc.arg(cutoff)::timestamptz
GROUP BY merchant_id ORDER BY min(recorded_at),merchant_id LIMIT sqlc.arg(row_limit)::int;

-- name: LockEventLogStream :exec
SELECT 1 FROM billing.event_log_streams WHERE merchant_id=sqlc.arg(merchant_id)::uuid FOR UPDATE;

-- name: PruneEventLog :one
WITH gone AS (
    DELETE FROM billing.event_log WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND position IN (
        SELECT position FROM billing.event_log WHERE merchant_id=sqlc.arg(merchant_id)::uuid
        AND recorded_at<sqlc.arg(cutoff)::timestamptz ORDER BY position LIMIT sqlc.arg(row_limit)::int
    ) RETURNING position,id
)
SELECT count(*)::bigint AS removed,coalesce(max(position),0)::bigint AS position,
coalesce((array_agg(id ORDER BY position DESC))[1],'')::text AS id FROM gone;

-- name: AdvanceEventLogFloor :exec
UPDATE billing.event_log_streams SET pruned_through=sqlc.arg(position)::bigint,pruned_id=sqlc.arg(id)::text
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND pruned_through<sqlc.arg(position)::bigint;
