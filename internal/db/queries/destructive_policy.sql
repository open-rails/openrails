-- Destructive-action kill switch and first-enforce gate. destructive_action_switch
-- is one instance-wide row; merchant_destructive_policy is per merchant.

-- name: GetDestructivePolicy :one
-- The effective policy for one merchant in one read: switch_enabled (instance
-- kill switch), merchant_enabled (per-merchant stop; no row = true) and
-- enforce_armed_at (NULL or no row = pulls run advisory).
SELECT
    COALESCE(s.enabled, false)::boolean AS switch_enabled,
    COALESCE(m.destructive_actions_enabled, true)::boolean AS merchant_enabled,
    m.enforce_armed_at,
    m.first_pull_completed_at
FROM (SELECT 1) AS one
LEFT JOIN billing.destructive_action_switch s ON true
LEFT JOIN billing.merchant_destructive_policy m ON m.merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: IsDestructiveActionSwitchEnabled :one
SELECT COALESCE((SELECT enabled FROM billing.destructive_action_switch LIMIT 1), false)::boolean AS enabled;

-- name: SetDestructiveActionSwitch :exec
-- One UPDATE halts every destructive plane on every node at its next gate check.
UPDATE billing.destructive_action_switch
SET enabled = sqlc.arg(enabled)::boolean,
    updated_by = sqlc.narg(updated_by)::text,
    reason = sqlc.narg(reason)::text,
    updated_at = now();

-- name: ArmMerchantEnforcement :exec
-- Arms a merchant for enforcing pulls once an operator has reviewed its first
-- advisory pull's findings.
INSERT INTO billing.merchant_destructive_policy (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, true, sqlc.arg(armed_at)::timestamptz, sqlc.narg(updated_by)::text, sqlc.narg(reason)::text, now())
ON CONFLICT (merchant_id) DO UPDATE SET
    enforce_armed_at = EXCLUDED.enforce_armed_at,
    destructive_actions_enabled = true,
    updated_by = EXCLUDED.updated_by,
    reason = EXCLUDED.reason,
    updated_at = now();

-- name: RecordFirstPullCompleted :exec
-- Stamped by the first completed advisory pull so an operator can see the
-- merchant has been surveyed and its findings are ready to review. Never
-- overwritten.
INSERT INTO billing.merchant_destructive_policy (merchant_id, first_pull_completed_at, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(completed_at)::timestamptz, now())
ON CONFLICT (merchant_id) DO UPDATE SET
    first_pull_completed_at = COALESCE(billing.merchant_destructive_policy.first_pull_completed_at, EXCLUDED.first_pull_completed_at),
    updated_at = now();

-- The merchant's live linked book on one rail: statuses that still bill or grant
-- access, with a rail handle. The cancellation cap and roster ratio breaker are
-- measured against it.
-- name: CountLiveLinkedSubscriptionsForRail :one
SELECT count(*) FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = sqlc.arg(rail)::text
  AND status IN ('active', 'past_due', 'awaiting_method', 'unverified')
  AND rail_subscription_id IS NOT NULL
  AND deleted_at IS NULL;
