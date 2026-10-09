-- name: GetWritePosture :one
-- A merchant's stored write posture ('full' when it has none) and whether
-- this database is the one the billing book was armed in.
SELECT
    COALESCE(p.mode, 'full')::text AS mode,
    p.reason,
    p.set_by,
    p.set_at,
    billing.book_armed()::boolean AS book_armed
FROM (SELECT 1) AS one
LEFT JOIN billing.merchant_write_posture p ON p.merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: GetBookArmed :one
SELECT billing.book_armed()::boolean AS book_armed;

-- name: ArmBook :exec
SELECT billing.arm_book(sqlc.arg(armed_by)::text, sqlc.arg(armed_at)::timestamptz);

-- name: SetWritePosture :exec
INSERT INTO billing.merchant_write_posture (merchant_id, mode, reason, set_by, set_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(mode)::text, sqlc.arg(reason)::text, sqlc.arg(set_by)::text, sqlc.arg(set_at)::timestamptz)
ON CONFLICT (merchant_id) DO UPDATE SET
    mode = EXCLUDED.mode,
    reason = EXCLUDED.reason,
    set_by = EXCLUDED.set_by,
    set_at = EXCLUDED.set_at;

-- name: LockWritePosture :one
SELECT mode, reason, set_by, set_at FROM billing.merchant_write_posture
WHERE merchant_id = sqlc.arg(merchant_id)::uuid FOR UPDATE;

-- name: DeleteWritePosture :exec
DELETE FROM billing.merchant_write_posture WHERE merchant_id = sqlc.arg(merchant_id)::uuid;
