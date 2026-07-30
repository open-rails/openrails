-- or#859 tier 1, slice 2: converge-enforce reversibility.
--
-- or#858's prune reverses DELETEs (`SET deleted_at = NULL WHERE
-- destructive_run_id = $1`). These reverse UPDATEs, which is the damage the
-- empty-roster mass cancellation actually did, plus the provider writes it
-- queued behind them.

-- --- capture (inside the run, immediately before each write) ------------------

-- name: CaptureSubscriptionBeforeImage :execrows
-- The row verbatim, server-side, so the image cannot drift from the table.
-- ON CONFLICT DO NOTHING: the FIRST capture inside a run is the state the run
-- inherited; a later one would be the run's own write.
INSERT INTO openrails.destructive_run_before_images (
    merchant_id, destructive_run_id, table_name, row_id, before, captured_at
)
SELECT s.merchant_id, sqlc.arg(run_id)::uuid, 'subscriptions', s.id, to_jsonb(s), sqlc.arg(now)::timestamptz
FROM openrails.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND s.id = sqlc.arg(subscription_id)::uuid
ON CONFLICT (destructive_run_id, table_name, row_id) DO NOTHING;

-- name: CaptureSubscriptionEntitlementBeforeImages :execrows
-- Every LIVE entitlement window the transition is about to revoke or bound.
-- Captured as EVIDENCE only: the reverse never replays these (or#859 §3.3 —
-- entitlements are Class D, recomputed by Converge from the append-only grant
-- log, never restored; a restored effect can silently disagree with its grant,
-- a re-derived one cannot). What they buy is the ability to say exactly which
-- windows a bad pass closed, and to check the recomputation against them.
INSERT INTO openrails.destructive_run_before_images (
    merchant_id, destructive_run_id, table_name, row_id, before, captured_at
)
SELECT e.merchant_id, sqlc.arg(run_id)::uuid, 'entitlements', e.id, to_jsonb(e), sqlc.arg(now)::timestamptz
FROM openrails.entitlements e
WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid
  AND e.source_type = 'subscription'
  AND e.source_id = sqlc.arg(subscription_id)::uuid
  AND e.revoked_at IS NULL
  AND e.deleted_at IS NULL
ON CONFLICT (destructive_run_id, table_name, row_id) DO NOTHING;

-- --- intent attribution -------------------------------------------------------

-- name: StampRailIntentsForRun :execrows
-- Attribute the provider writes this pass queued for one subscription to the
-- run that queued them. `since` is the instant the run captured that
-- subscription's before-image, so anything newer for that subject is this
-- pass's doing; `destructive_run_id IS NULL` keeps an earlier run's intent from
-- being re-attributed. Attribution only — no status is changed here.
UPDATE openrails.rail_intents
SET destructive_run_id = sqlc.arg(run_id)::uuid
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND destructive_run_id IS NULL
  AND created_at >= sqlc.arg(since)::timestamptz;

-- name: SupersedeUnfiredRailIntentsForRun :many
-- STEP ONE of the reverse, before a single row is restored, because it is the
-- only step racing a live actor: the intent runner may claim a queued NMI vault
-- delete at any moment.
--
-- Only `pending` and `failed_retryable` are unfired. `in_flight` is leased by an
-- executor that may already be on the wire; `unknown_needs_verify` means an
-- attempt was MADE and its outcome is unresolved. Neither may be called undone,
-- so neither is touched here — they are reported instead.
--
-- The race is decided by Postgres row locks: this UPDATE and the executor's
-- claim (ClaimDueRailIntents / ClaimRailIntentByID) contend for the same row,
-- and under READ COMMITTED the loser re-evaluates its WHERE against the winner's
-- committed row and matches nothing. So exactly one of {superseded, in_flight}
-- happens per intent, never both, and whichever way it goes the reverse's
-- report is truthful. The reverse also disarms the destructive-action switch
-- first, which is what stops NEW claims from starting during the reversal.
UPDATE openrails.rail_intents
SET status = 'superseded',
    last_failure_reason = sqlc.arg(reason)::text,
    claimed_until = NULL,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND destructive_run_id = sqlc.arg(run_id)::uuid
  AND status IN ('pending', 'failed_retryable')
RETURNING id, intent_type, subscription_id, rail;

-- name: ListRailIntentsForRun :many
-- The divergence manifest. Read AFTER the supersede so every row's status is
-- final: superseded = neutralised; succeeded = it reached the provider and is
-- IRREVERSIBLE (the vault entry is gone, the remote subscription is cancelled);
-- in_flight / unknown_needs_verify = ambiguous, may have reached the provider.
SELECT id, intent_type, status, subscription_id, rail, executed_at, last_failure_reason
FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND destructive_run_id = sqlc.arg(run_id)::uuid
ORDER BY created_at;

-- --- restore ------------------------------------------------------------------

-- name: RestoreSubscriptionsFromBeforeImages :execrows
-- Re-assert the columns a converge-enforce pass can move, and only those.
--
-- Upsert-shaped by necessity, not by preference: `grants` FK-pins the payments
-- and products it justifies, so a rollback physically cannot delete-and-reinsert
-- (or#859 §2.3; merchant purge already hit this wall as
-- ErrPurgeBlockedByRetainedHistory). It is also the safer shape — a whole-row
-- rewrite from the image would re-stamp identity and FK columns this run never
-- touched, and clobber whatever another plane legitimately advanced meanwhile.
--
-- Not restored on purpose: deleted_at / destructive_run_id (or#858's soft-delete
-- pair, owned by prune — a converge run must never resurrect a pruned row), and
-- psp_id / customer_id / product_id / price_id (identity, which no transition
-- moves).
UPDATE openrails.subscriptions s
SET status                   = (b.before->>'status')::openrails.subscription_status,
    current_period_starts_at = (b.before->>'current_period_starts_at')::timestamptz,
    current_period_ends_at   = (b.before->>'current_period_ends_at')::timestamptz,
    ended_at                 = (b.before->>'ended_at')::timestamptz,
    grace_ends_at            = (b.before->>'grace_ends_at')::timestamptz,
    last_retry_at            = (b.before->>'last_retry_at')::timestamptz,
    retry_attempts           = (b.before->>'retry_attempts')::integer,
    next_retry_at            = (b.before->>'next_retry_at')::timestamptz,
    cancelled_at             = (b.before->>'cancelled_at')::timestamptz,
    cancel_type              = b.before->>'cancel_type',
    cancel_feedback          = b.before->>'cancel_feedback',
    deletion_scheduled_at    = (b.before->>'deletion_scheduled_at')::timestamptz,
    updated_at               = sqlc.arg(now)::timestamptz
FROM openrails.destructive_run_before_images b
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
  AND b.destructive_run_id = sqlc.arg(run_id)::uuid
  AND b.table_name = 'subscriptions'
  AND b.restored_at IS NULL
  AND s.merchant_id = b.merchant_id
  AND s.id = b.row_id;

-- name: MarkBeforeImagesRestored :execrows
-- Runs in the same transaction as the restore above. Entitlement images are
-- deliberately excluded: leaving restored_at NULL on them is the durable record
-- that the reverse saw them and chose recomputation over restoration.
UPDATE openrails.destructive_run_before_images
SET restored_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND destructive_run_id = sqlc.arg(run_id)::uuid
  AND table_name = sqlc.arg(table_name)::text
  AND restored_at IS NULL;

-- name: CountBeforeImagesForRun :one
SELECT
    count(*) FILTER (WHERE table_name = 'subscriptions')::bigint AS subscriptions,
    count(*) FILTER (WHERE table_name = 'entitlements')::bigint AS entitlements
FROM openrails.destructive_run_before_images
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND destructive_run_id = sqlc.arg(run_id)::uuid;

-- --- post-rollback quiesce / re-arm gates ------------------------------------

-- name: ResetReconciliationStateUnproven :execrows
-- or#859 §2.2(1): after a rollback the book is definitionally incomplete, so a
-- stale `fully_reconciled = true` licenses mass retraction against a book that
-- is missing rows — i.e. it re-creates the very incident being recovered from.
-- The single most dangerous post-rollback state in the system; cleared here.
UPDATE openrails.reconciliation_state
SET fully_reconciled = false, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND fully_reconciled = true;

-- name: DisarmMerchantEnforcement :exec
-- or#859 §2.2(2): clear the #835 first-enforce arming so the post-rollback pull
-- runs ADVISORY — findings persisted, nothing mutated — until an operator reads
-- them and re-arms by hand. Also trips the per-merchant destructive stop, which
-- is the quiesce half: it is read by the same gate the intent runner checks, so
-- no new provider write starts while the reversal runs.
INSERT INTO openrails.merchant_destructive_policy (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, false, NULL, sqlc.narg(updated_by)::text, sqlc.narg(reason)::text, now())
ON CONFLICT (merchant_id) DO UPDATE SET
    destructive_actions_enabled = false,
    enforce_armed_at = NULL,
    updated_by = EXCLUDED.updated_by,
    reason = EXCLUDED.reason,
    updated_at = now();
