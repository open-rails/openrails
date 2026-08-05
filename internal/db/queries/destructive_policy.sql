-- #836 destructive-action kill switch / #835 first-enforce gate.
--
-- destructive_action_switch is instance-level and RLS-exempt so the no-GUC
-- background connections (intent runner, sweep scheduler) can read it — a kill
-- switch scoped by the connection it polices is not a kill switch.
-- merchant_destructive_policy is ordinary RLS-protected tenant data.

-- name: GetDestructivePolicy :one
-- The effective policy for one merchant in ONE read:
--   switch_enabled   the instance kill switch (false = everything destructive halts)
--   merchant_enabled the per-merchant stop; no row = inherit (true)
--   enforce_armed_at #835: NULL (including "no row") = this merchant's pulls run advisory
-- Must be run merchant-scoped: merchant_destructive_policy is RLS-protected.
SELECT
    COALESCE(s.enabled, false)::boolean AS switch_enabled,
    COALESCE(m.destructive_actions_enabled, true)::boolean AS merchant_enabled,
    m.enforce_armed_at,
    m.first_pull_completed_at
FROM (SELECT 1) AS one
LEFT JOIN openrails.destructive_action_switch s ON true
LEFT JOIN openrails.merchant_destructive_policy m ON m.merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: IsDestructiveActionSwitchEnabled :one
SELECT COALESCE((SELECT enabled FROM openrails.destructive_action_switch LIMIT 1), false)::boolean AS enabled;

-- name: SetDestructiveActionSwitch :exec
-- The 3am kill switch: one UPDATE halts every destructive plane on every node
-- at its next gate check — no deploy, no restart, no scaling workers to zero.
UPDATE openrails.destructive_action_switch
SET enabled = sqlc.arg(enabled)::boolean,
    updated_by = sqlc.narg(updated_by)::text,
    reason = sqlc.narg(reason)::text,
    updated_at = now();

-- name: SetMerchantDestructiveActionsEnabled :exec
INSERT INTO openrails.merchant_destructive_policy (merchant_id, destructive_actions_enabled, updated_by, reason, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(enabled)::boolean, sqlc.narg(updated_by)::text, sqlc.narg(reason)::text, now())
ON CONFLICT (merchant_id) DO UPDATE SET
    destructive_actions_enabled = EXCLUDED.destructive_actions_enabled,
    updated_by = EXCLUDED.updated_by,
    reason = EXCLUDED.reason,
    updated_at = now();

-- name: ArmMerchantEnforcement :exec
-- #835: bless a merchant for ENFORCING pulls, after an operator reviewed the
-- findings its first advisory pull produced.
INSERT INTO openrails.merchant_destructive_policy (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason, updated_at)
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
INSERT INTO openrails.merchant_destructive_policy (merchant_id, first_pull_completed_at, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(completed_at)::timestamptz, now())
ON CONFLICT (merchant_id) DO UPDATE SET
    first_pull_completed_at = COALESCE(openrails.merchant_destructive_policy.first_pull_completed_at, EXCLUDED.first_pull_completed_at),
    updated_at = now();

-- #834/#837 denominator: the merchant's LIVE linked book on one rail. Live =
-- the statuses that still bill or still grant access; linked = it carries a
-- rail handle, so provider absence could be read as death. Both the
-- cancellation cap and the roster ratio breaker are measured against this.
-- name: CountLiveLinkedSubscriptionsForRail :one
SELECT count(*) FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = sqlc.arg(rail)::text
  AND status IN ('active', 'past_due', 'unknown')
  AND rail_subscription_id <> ''
  AND deleted_at IS NULL;
