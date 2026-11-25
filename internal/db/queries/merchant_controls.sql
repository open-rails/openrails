-- name: GetMerchantControls :one
-- A merchant's controls (writes full, rebills released and destructive
-- actions disarmed when it has no row) and whether this database is the one
-- the billing book was armed in.
SELECT
    COALESCE(c.writes, 'full')::text AS writes,
    COALESCE(c.rebills_held, false)::boolean AS rebills_held,
    COALESCE(c.destructive_armed, false)::boolean AS destructive_armed,
    c.first_pull_completed_at,
    c.changed_by,
    c.reason,
    c.changed_at,
    billing.book_armed()::boolean AS book_armed
FROM (SELECT 1) AS one
LEFT JOIN billing.merchant_controls c ON c.merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: LockMerchantControls :exec
-- Serializes one merchant's control changes, so each records what it changed.
SELECT pg_advisory_xact_lock(hashtextextended('openrails.merchant_controls:' || sqlc.arg(merchant_id)::uuid::text, 0));

-- name: SetMerchantControls :exec
INSERT INTO billing.merchant_controls (merchant_id, writes, rebills_held, destructive_armed, changed_by, reason, changed_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(writes)::text, sqlc.arg(rebills_held)::boolean, sqlc.arg(destructive_armed)::boolean,
    sqlc.arg(changed_by)::text, sqlc.arg(reason)::text, sqlc.arg(changed_at)::timestamptz)
ON CONFLICT (merchant_id) DO UPDATE SET
    writes = EXCLUDED.writes,
    rebills_held = EXCLUDED.rebills_held,
    destructive_armed = EXCLUDED.destructive_armed,
    changed_by = EXCLUDED.changed_by,
    reason = EXCLUDED.reason,
    changed_at = EXCLUDED.changed_at;

-- name: RecordFirstPullCompleted :exec
-- Stamped by the first completed pull, so an operator can see the merchant
-- has been surveyed and its findings are ready to review. Never overwritten;
-- the controls are left as they are.
INSERT INTO billing.merchant_controls (merchant_id, writes, rebills_held, destructive_armed, first_pull_completed_at)
VALUES (sqlc.arg(merchant_id)::uuid, 'full', false, false, sqlc.arg(completed_at)::timestamptz)
ON CONFLICT (merchant_id) DO UPDATE SET
    first_pull_completed_at = COALESCE(billing.merchant_controls.first_pull_completed_at, EXCLUDED.first_pull_completed_at);

-- name: ListMerchantControls :many
-- CROSS-MERCHANT: every merchant in the directory with its controls, by name.
SELECT m.id AS merchant_id, m.slug,
    COALESCE(c.writes, 'full')::text AS writes,
    COALESCE(c.rebills_held, false)::boolean AS rebills_held,
    COALESCE(c.destructive_armed, false)::boolean AS destructive_armed,
    c.first_pull_completed_at,
    c.changed_by,
    c.reason,
    c.changed_at
FROM billing.merchants m
LEFT JOIN billing.merchant_controls c ON c.merchant_id = m.id
WHERE m.deleted_at IS NULL
ORDER BY m.slug;

-- name: GetBookArmed :one
SELECT billing.book_armed()::boolean AS book_armed;

-- name: ArmBook :exec
SELECT billing.arm_book(sqlc.arg(armed_by)::text, sqlc.arg(armed_at)::timestamptz);

-- name: GetBookTag :one
SELECT book_id, tagging_started_at FROM billing.book_identity WHERE singleton;
