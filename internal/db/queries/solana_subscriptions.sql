-- billing.solana_subscriptions: on-chain recurring subscription state.

-- name: UpsertSolanaSubscription :exec
INSERT INTO billing.solana_subscriptions (
    id, merchant_id, subscription_id, subscriber_wallet, authority_pda,
    subscription_pda, plan_pda, merchant_address, mint,
    plan_created_at_fingerprint, last_pulled_period_starts_at, last_signature,
    next_pull_at, status, created_at, updated_at, treasury
) VALUES (
    $1,
    sqlc.arg(merchant_id)::uuid,
    $2, $3, $4, $5, $6, $7, $8, $9,
    sqlc.narg(last_pulled_period_starts_at), sqlc.narg(last_signature),
    $10, $11, $12, $13, sqlc.arg(treasury)::text
)
ON CONFLICT (subscription_pda) DO UPDATE SET
    subscription_id = EXCLUDED.subscription_id,
    next_pull_at = EXCLUDED.next_pull_at,
    status = EXCLUDED.status,
    plan_created_at_fingerprint = EXCLUDED.plan_created_at_fingerprint,
    updated_at = EXCLUDED.updated_at
WHERE billing.solana_subscriptions.merchant_id = EXCLUDED.merchant_id;

-- name: GetSolanaSubscriptionByPDA :one
SELECT * FROM billing.solana_subscriptions WHERE solana_subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_pda = $1;

-- name: GetSolanaSubscriptionBySubscriptionID :one
SELECT * FROM billing.solana_subscriptions WHERE solana_subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = $1;

-- name: ListDueSolanaSubscriptions :many
-- The merchant's subscriptions due a pull, each with the PSP it pulls on.
SELECT sqlc.embed(s), sub.psp_id
FROM billing.solana_subscriptions s
JOIN billing.subscriptions sub ON sub.id = s.subscription_id
-- A subscription a prune tombstoned is not due: the join is a live read. The
-- parent terminal guard is defence in depth should a cascade leave its mirror
-- active.
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND sub.merchant_id = sqlc.arg(merchant_id)::uuid AND s.status = 'active' AND s.next_pull_at <= sqlc.arg(now)::timestamptz
  AND sub.deleted_at IS NULL
  AND sub.status <> 'canceled'
ORDER BY s.merchant_id ASC, s.next_pull_at ASC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0);

-- name: GetSolanaPullEpisode :one
-- The row's due pull episode and the PSP it pulls on: an active row whose
-- subscription is live. None is no pull to schedule.
SELECT s.id, s.subscription_id, s.subscription_pda, s.next_pull_at, sub.psp_id
FROM billing.solana_subscriptions s
JOIN billing.subscriptions sub ON sub.merchant_id = s.merchant_id AND sub.id = s.subscription_id
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.id = sqlc.arg(id)::uuid AND s.status = 'active'
  AND sub.deleted_at IS NULL AND sub.status <> 'canceled';

-- name: AdvanceSolanaSubscriptionAfterPull :exec
UPDATE billing.solana_subscriptions SET
    last_pulled_period_starts_at = $2,
    last_signature = $3,
    next_pull_at = $4,
    updated_at = $5
WHERE solana_subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND (next_pull_at = sqlc.arg(episode_at)::timestamptz OR last_signature = $3)
  AND (last_pulled_period_starts_at IS NULL OR last_pulled_period_starts_at <= $2);

-- name: SetSolanaSubscriptionNextPullAt :exec
UPDATE billing.solana_subscriptions SET
    next_pull_at = $2,
    updated_at = $3
WHERE solana_subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- name: ListActiveSolanaMerchantWallets :many
-- Each plan owner with active subscriptions: how many, on how many plans.
SELECT s.merchant_id, sub.psp_id, s.merchant_address, count(*)::bigint AS subscriptions, count(DISTINCT s.plan_pda)::bigint AS plans
FROM billing.solana_subscriptions s
JOIN billing.subscriptions sub ON sub.id = s.subscription_id AND sub.merchant_id = s.merchant_id
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.status = 'active'
  AND sub.deleted_at IS NULL AND sub.status <> 'canceled'
GROUP BY s.merchant_id, sub.psp_id, s.merchant_address;

-- name: SetSolanaSubscriptionStatus :exec
UPDATE billing.solana_subscriptions SET
    status = $2,
    updated_at = $3
WHERE solana_subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;
