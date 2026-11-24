-- name: LockRevenueSchedule :one
-- A schedule a reversal adjusts, locked against the recognition job.
SELECT * FROM billing.revenue_schedules
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: AdjustRevenueSchedule :execrows
-- A reversal's change to a schedule: what it is funded with and has
-- recognised, as of when, and when it is next due.
UPDATE billing.revenue_schedules
SET funded = sqlc.arg(funded)::bigint,
    recognized_net = sqlc.arg(recognized_net)::bigint,
    recognized_through_at = sqlc.arg(recognized_through_at)::timestamptz,
    next_recognition_at = sqlc.narg(next_recognition_at)::timestamptz,
    reinstatements = sqlc.arg(reinstatements)::int,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;
