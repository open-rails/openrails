-- openrails.worker_health (#689) — operator-global, no merchant scope.

-- name: SeedWorkerHealth :exec
INSERT INTO openrails.worker_health (worker_kind, expected_period_seconds)
VALUES ($1, sqlc.narg(expected_period_seconds))
ON CONFLICT (worker_kind) DO UPDATE
SET expected_period_seconds = EXCLUDED.expected_period_seconds;

-- name: RecordWorkerSuccess :exec
INSERT INTO openrails.worker_health (worker_kind, last_success_at, consecutive_failures, updated_at)
VALUES ($1, sqlc.arg(now)::timestamptz, 0, sqlc.arg(now)::timestamptz)
ON CONFLICT (worker_kind) DO UPDATE
SET last_success_at = EXCLUDED.last_success_at,
    consecutive_failures = 0,
    updated_at = EXCLUDED.updated_at;

-- name: RecordWorkerFailure :exec
INSERT INTO openrails.worker_health (worker_kind, last_error_at, last_error, consecutive_failures, updated_at)
VALUES ($1, sqlc.arg(now)::timestamptz, sqlc.arg(last_error), 1, sqlc.arg(now)::timestamptz)
ON CONFLICT (worker_kind) DO UPDATE
SET last_error_at = EXCLUDED.last_error_at,
    last_error = EXCLUDED.last_error,
    consecutive_failures = openrails.worker_health.consecutive_failures + 1,
    updated_at = EXCLUDED.updated_at;

-- name: ListWorkerHealth :many
SELECT * FROM openrails.worker_health ORDER BY worker_kind;

-- name: MarkWorkerHealthAlerted :exec
UPDATE openrails.worker_health
SET last_alerted_at = sqlc.arg(now)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE worker_kind = $1;
