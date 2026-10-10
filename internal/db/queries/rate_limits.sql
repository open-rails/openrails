-- Adds hits to key's window, which ends at expires_at when it opens. A lapsed
-- window under the same key opens again.
-- name: HitRateWindow :one
INSERT INTO billing.rate_windows (key, hits, expires_at)
VALUES (sqlc.arg(key)::text, sqlc.arg(hits)::bigint, sqlc.arg(expires_at)::timestamptz)
ON CONFLICT (key) DO UPDATE SET
    hits = CASE WHEN billing.rate_windows.expires_at <= now() THEN EXCLUDED.hits ELSE billing.rate_windows.hits + EXCLUDED.hits END,
    expires_at = CASE WHEN billing.rate_windows.expires_at <= now() THEN EXCLUDED.expires_at ELSE billing.rate_windows.expires_at END
RETURNING hits, expires_at;

-- Holds key until expires_at: a lockout or a captcha challenge.
-- name: MarkRateWindow :exec
INSERT INTO billing.rate_windows (key, hits, expires_at)
VALUES (sqlc.arg(key)::text, 1, sqlc.arg(expires_at)::timestamptz)
ON CONFLICT (key) DO UPDATE SET hits = 1, expires_at = EXCLUDED.expires_at;

-- When a live key ends; no row when it is not held.
-- name: LiveRateWindow :one
SELECT expires_at FROM billing.rate_windows
WHERE key = sqlc.arg(key)::text AND expires_at > now();

-- name: ClearRateWindows :exec
DELETE FROM billing.rate_windows WHERE key = ANY(sqlc.arg(keys)::text[]);

-- Deletes up to row_limit expired keys, oldest first.
-- name: PruneRateWindows :execrows
DELETE FROM billing.rate_windows
WHERE key IN (
    SELECT key FROM billing.rate_windows
    WHERE expires_at <= now()
    ORDER BY expires_at
    LIMIT sqlc.arg(row_limit)::int
);
