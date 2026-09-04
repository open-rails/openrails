-- name: GetAutoTopupEpisode :one
SELECT * FROM openrails.auto_topup_episodes
WHERE merchant_id = $1 AND intent_id = $2;

-- name: InsertAutoTopupEpisode :one
INSERT INTO openrails.auto_topup_episodes (intent_id, merchant_id, customer_id, currency, reserved_at, amount_native)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: AutoTopupSafetyCounts :one
SELECT count(*) FILTER (WHERE reserved_at > sqlc.arg(day_start)::timestamptz)::bigint AS daily,
       count(*) FILTER (WHERE reserved_at > sqlc.arg(week_start)::timestamptz)::bigint AS weekly,
       count(*) FILTER (WHERE reserved_at > sqlc.arg(month_start)::timestamptz)::bigint AS monthly,
       count(*) FILTER (WHERE finalized_at IS NULL)::bigint AS pending
FROM openrails.auto_topup_episodes
WHERE merchant_id = $1 AND customer_id = $2 AND currency = $3
  AND (reserved_at > sqlc.arg(month_start)::timestamptz OR finalized_at IS NULL);

-- name: RecordAutoTopupReceipt :execrows
UPDATE openrails.auto_topup_episodes SET receipt = sqlc.arg(receipt)::jsonb
WHERE merchant_id = $1 AND intent_id = $2 AND receipt IS NULL AND finalized_at IS NULL;

-- name: FinalizeAutoTopupEpisode :execrows
UPDATE openrails.auto_topup_episodes SET finalized_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND intent_id = $2 AND finalized_at IS NULL AND receipt IS NOT NULL;

-- name: CompleteAutoTopupAccount :exec
UPDATE openrails.money_settings
SET last_topup_at = sqlc.arg(now)::timestamptz,
    auto_topup_failures = sqlc.arg(failures)::integer,
    auto_topup_enabled = CASE WHEN sqlc.arg(disable)::boolean THEN false ELSE auto_topup_enabled END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND currency = $3;

-- name: ResetAutoTopupFailures :exec
UPDATE openrails.money_settings SET auto_topup_failures = 0
WHERE merchant_id = $1 AND customer_id = $2 AND currency = $3;
