-- openrails.worker_state (#689) — operator-global, no merchant scope.
--
-- Health writes are monotonic. Job completions reach the row out of order
-- (concurrent completions of one kind, a late-finishing attempt), so a write
-- may only add what is newer than the row already holds: timestamps never
-- move backwards, last_error is the text of the newest failure, a success
-- resets the streak only when no newer failure is recorded and a failure
-- counts only when no newer success is recorded. Under reordering the streak
-- never under-counts (an alert is never suppressed); it can over-count by the
-- stale failures a late success could not retract.

-- name: SeedWorkerHealth :exec
INSERT INTO openrails.worker_state (worker_kind, expected_period_seconds)
VALUES ($1, sqlc.narg(expected_period_seconds))
ON CONFLICT (worker_kind) DO UPDATE
SET expected_period_seconds = EXCLUDED.expected_period_seconds;

-- name: RecordWorkerSuccess :exec
INSERT INTO openrails.worker_state (worker_kind, last_success_at, consecutive_failures, updated_at)
VALUES ($1, sqlc.arg(now)::timestamptz, 0, sqlc.arg(now)::timestamptz)
ON CONFLICT (worker_kind) DO UPDATE
SET last_success_at = GREATEST(openrails.worker_state.last_success_at, EXCLUDED.last_success_at),
    consecutive_failures = CASE
        WHEN openrails.worker_state.last_error_at IS NULL
          OR EXCLUDED.last_success_at >= openrails.worker_state.last_error_at THEN 0
        ELSE openrails.worker_state.consecutive_failures
    END,
    updated_at = GREATEST(openrails.worker_state.updated_at, EXCLUDED.updated_at);

-- name: RecordWorkerFailure :exec
INSERT INTO openrails.worker_state (worker_kind, last_error_at, last_error, consecutive_failures, updated_at)
VALUES ($1, sqlc.arg(now)::timestamptz, sqlc.arg(last_error), 1, sqlc.arg(now)::timestamptz)
ON CONFLICT (worker_kind) DO UPDATE
SET last_error = CASE
        WHEN openrails.worker_state.last_error_at IS NULL
          OR EXCLUDED.last_error_at >= openrails.worker_state.last_error_at THEN EXCLUDED.last_error
        ELSE openrails.worker_state.last_error
    END,
    last_error_at = GREATEST(openrails.worker_state.last_error_at, EXCLUDED.last_error_at),
    consecutive_failures = CASE
        WHEN openrails.worker_state.last_success_at IS NULL
          OR EXCLUDED.last_error_at >= openrails.worker_state.last_success_at
        THEN openrails.worker_state.consecutive_failures + 1
        ELSE openrails.worker_state.consecutive_failures
    END,
    updated_at = GREATEST(openrails.worker_state.updated_at, EXCLUDED.updated_at);

-- name: ListWorkerHealth :many
SELECT * FROM openrails.worker_state ORDER BY worker_kind;

-- name: MarkWorkerHealthAlerted :exec
UPDATE openrails.worker_state
SET last_alerted_at = GREATEST(last_alerted_at, sqlc.arg(now)::timestamptz),
    updated_at = GREATEST(updated_at, sqlc.arg(now)::timestamptz)
WHERE worker_kind = $1;
