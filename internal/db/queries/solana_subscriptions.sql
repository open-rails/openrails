-- openrails.solana_subscriptions — on-chain recurring subscription state (#255).

-- name: UpsertSolanaSubscription :exec
INSERT INTO openrails.solana_subscriptions (
    id, tenant_id, subscription_id, subscriber_wallet, authority_pda,
    subscription_pda, plan_pda, merchant_address, mint,
    plan_created_at_fingerprint, last_pulled_period_start, last_signature,
    next_pull_at, status, created_at, updated_at
) VALUES (
    $1,
    COALESCE(NULLIF(sqlc.arg(tenant_id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid),
             '00000000-0000-0000-0000-000000000001'::uuid),
    $2, $3, $4, $5, $6, $7, $8, $9,
    sqlc.narg(last_pulled_period_start), sqlc.narg(last_signature),
    $10, $11, $12, $13
)
ON CONFLICT (subscription_pda) DO UPDATE SET
    subscription_id = EXCLUDED.subscription_id,
    next_pull_at = EXCLUDED.next_pull_at,
    status = EXCLUDED.status,
    plan_created_at_fingerprint = EXCLUDED.plan_created_at_fingerprint,
    updated_at = EXCLUDED.updated_at;

-- name: GetSolanaSubscriptionByPDA :one
SELECT * FROM openrails.solana_subscriptions WHERE subscription_pda = $1;

-- name: GetSolanaSubscriptionBySubscriptionID :one
SELECT * FROM openrails.solana_subscriptions WHERE subscription_id = $1;

-- name: ListDueSolanaSubscriptions :many
SELECT * FROM openrails.solana_subscriptions
WHERE status = 'active' AND next_pull_at <= sqlc.arg(now)::timestamptz
ORDER BY tenant_id ASC, next_pull_at ASC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0);

-- name: AdvanceSolanaSubscriptionAfterPull :exec
UPDATE openrails.solana_subscriptions SET
    last_pulled_period_start = $2,
    last_signature = $3,
    next_pull_at = $4,
    updated_at = $5
WHERE id = $1;

-- name: SetSolanaSubscriptionNextPullAt :exec
UPDATE openrails.solana_subscriptions SET
    next_pull_at = $2,
    updated_at = $3
WHERE id = $1;

-- name: ListActiveSolanaMerchantWallets :many
SELECT DISTINCT tenant_id, merchant_address
FROM openrails.solana_subscriptions
WHERE status = 'active';

-- name: ListActiveSolanaSubscriptionsWithSignature :many
SELECT * FROM openrails.solana_subscriptions
WHERE status = 'active'
  AND last_signature IS NOT NULL
  AND last_signature <> ''
ORDER BY updated_at ASC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0);

-- name: SetSolanaSubscriptionStatus :exec
UPDATE openrails.solana_subscriptions SET
    status = $2,
    updated_at = $3
WHERE id = $1;
