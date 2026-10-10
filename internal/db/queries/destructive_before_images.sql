-- Converge-enforce reversibility: before-images captured inside the run right
-- before each update, and the provider writes it queued. Prune reverses by
-- clearing deleted_at.

-- name: CaptureSubscriptionBeforeImage :execrows
-- The row verbatim, server-side, so the image cannot drift from the table.
-- ON CONFLICT DO NOTHING: the FIRST capture inside a run is the state the run
-- inherited; a later one would be the run's own write.
INSERT INTO billing.destructive_run_before_images (
    merchant_id, destructive_run_id, table_name, row_id, before, captured_at
)
SELECT s.merchant_id, sqlc.arg(run_id)::uuid, 'subscriptions', s.id, to_jsonb(s), sqlc.arg(now)::timestamptz
FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND s.id = sqlc.arg(subscription_id)::uuid
  -- A pruned row is prune's to reverse, never converge's.
  AND s.deleted_at IS NULL
ON CONFLICT (merchant_id, destructive_run_id, table_name, row_id) DO NOTHING;

-- name: CaptureSubscriptionAccessBeforeImages :execrows
-- Every live access window the transition is about to revoke or bound. Evidence
-- only: the reverse never replays these, Converge re-derives access from the
-- grant log. They show which windows a bad pass closed.
INSERT INTO billing.destructive_run_before_images (
    merchant_id, destructive_run_id, table_name, row_id, before, captured_at
)
SELECT e.merchant_id, sqlc.arg(run_id)::uuid, 'product_access', e.id, to_jsonb(e), sqlc.arg(now)::timestamptz
FROM billing.product_access e
WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid
  AND e.source_type = 'subscription'
  AND e.source_id = sqlc.arg(subscription_id)::uuid::text
  AND e.revoked_at IS NULL
  AND e.deleted_at IS NULL
ON CONFLICT (merchant_id, destructive_run_id, table_name, row_id) DO NOTHING;

-- name: StampProviderIntentsForRun :execrows
-- Attributes to the run the provider writes it queued for one subscription:
-- unattributed intents created since its before-image (`since`).
UPDATE billing.provider_intents
SET destructive_run_id = sqlc.arg(run_id)::uuid
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND destructive_run_id IS NULL
  AND created_at >= sqlc.arg(since)::timestamptz;

-- name: SupersedeUnfiredProviderIntentsForRun :many
-- First step of the reverse, racing the intent runner: supersedes the run's
-- unfired intents (pending, failed_retryable). in_flight and
-- unknown_needs_verify may have reached the provider and are reported instead.
-- Row locks decide the race with the executor's claim: under READ COMMITTED the
-- loser re-checks its WHERE and matches nothing, so each intent ends superseded
-- or in_flight, never both. The merchant's destructive stop, tripped
-- beforehand, keeps new claims from starting.
UPDATE billing.provider_intents
SET status = 'superseded',
    last_failure_reason = sqlc.arg(reason)::text,
    lease_expires_at = NULL,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND destructive_run_id = sqlc.arg(run_id)::uuid
  AND status IN ('pending', 'failed_retryable')
RETURNING id, intent_type, subscription_id, rail;

-- name: ListProviderIntentsForRun :many
-- The divergence manifest, read after the supersede: superseded = neutralised;
-- succeeded = reached the provider, irreversible; in_flight /
-- unknown_needs_verify = may have reached it.
SELECT id, intent_type, status, subscription_id, rail, executed_at, last_failure_reason
FROM billing.provider_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND destructive_run_id = sqlc.arg(run_id)::uuid
ORDER BY created_at;

-- name: StampSubscriptionAfterImage :execrows
-- Right after the run's own write: the revision the reverse requires to find.
UPDATE billing.destructive_run_before_images b
SET after_lifecycle_rev = s.lifecycle_rev
FROM billing.subscriptions s
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
  AND b.destructive_run_id = sqlc.arg(run_id)::uuid
  AND b.table_name = 'subscriptions'
  AND b.row_id = sqlc.arg(subscription_id)::uuid
  AND b.restored_at IS NULL
  AND s.merchant_id = b.merchant_id
  AND s.id = b.row_id
  AND s.deleted_at IS NULL;

-- name: RestoreSubscriptionsFromBeforeImages :execrows
-- Re-asserts the columns a converge-enforce pass can move, only on rows
-- unchanged since the run's own write: a row a renewal, cancel or payment moved
-- later keeps its state (restoring could make a paid period due again) and its
-- image stays unrestored. Updates in place: identity, FK columns and
-- deleted_at/destructive_run_id (prune's) are never rewritten.
WITH restorable AS (
    SELECT b.id, b.row_id, b.before
    FROM billing.destructive_run_before_images b
    JOIN billing.subscriptions s ON s.merchant_id = b.merchant_id AND s.id = b.row_id
    WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
      AND b.destructive_run_id = sqlc.arg(run_id)::uuid
      AND b.table_name = 'subscriptions'
      AND b.restored_at IS NULL
      AND s.lifecycle_rev = b.after_lifecycle_rev
      -- A row prune has since tombstoned is prune's to reverse.
      AND s.deleted_at IS NULL
    FOR UPDATE OF s
), marked AS (
    UPDATE billing.destructive_run_before_images i
    SET restored_at = sqlc.arg(now)::timestamptz
    FROM restorable r
    WHERE i.merchant_id = sqlc.arg(merchant_id)::uuid AND i.id = r.id
)
UPDATE billing.subscriptions s
SET lifecycle_rev            = s.lifecycle_rev + 1,
    status                   = (b.before->>'status'),
    current_period_starts_at = (b.before->>'current_period_starts_at')::timestamptz,
    current_period_ends_at   = (b.before->>'current_period_ends_at')::timestamptz,
    ended_at                 = (b.before->>'ended_at')::timestamptz,
    grace_ends_at            = (b.before->>'grace_ends_at')::timestamptz,
    last_retry_at            = (b.before->>'last_retry_at')::timestamptz,
    retry_attempts           = (b.before->>'retry_attempts')::integer,
    next_retry_at            = (b.before->>'next_retry_at')::timestamptz,
    canceled_at             = (b.before->>'canceled_at')::timestamptz,
    cancel_type              = b.before->>'cancel_type',
    cancel_feedback          = b.before->>'cancel_feedback',
    deletion_scheduled_at    = (b.before->>'deletion_scheduled_at')::timestamptz,
    updated_at               = sqlc.arg(now)::timestamptz
FROM restorable b
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND s.id = b.row_id;

-- name: InvalidateAccessFromBeforeImages :execrows
-- Soft-deletes and stamps the access windows this run captured; Converge then
-- rebuilds them from the grant log (a re-derived window cannot disagree with its
-- grant, a restored one can). Left in place, a bounded but unrevoked window
-- would read as already present and block the rebuild. Uniques ignore
-- soft-deleted rows, so the rebuilt window does not collide.
UPDATE billing.product_access e
SET deleted_at = sqlc.arg(now)::timestamptz,
    destructive_run_id = sqlc.arg(run_id)::uuid,
    updated_at = sqlc.arg(now)::timestamptz
FROM billing.destructive_run_before_images b
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
  AND b.destructive_run_id = sqlc.arg(run_id)::uuid
  AND b.table_name = 'product_access'
  AND e.merchant_id = b.merchant_id
  AND e.id = b.row_id
  AND e.deleted_at IS NULL;

-- name: CountBeforeImagesForRun :one
SELECT
    count(*) FILTER (WHERE table_name = 'subscriptions')::bigint AS subscriptions,
    count(*) FILTER (WHERE table_name = 'subscriptions' AND restored_at IS NULL)::bigint AS subscriptions_unrestored,
    count(*) FILTER (WHERE table_name = 'product_access')::bigint AS product_access
FROM billing.destructive_run_before_images
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND destructive_run_id = sqlc.arg(run_id)::uuid;

-- name: ResetReconciliationStateUnproven :execrows
-- After a rollback the book is incomplete: a stale fully_reconciled = true would
-- license mass retraction against it.
UPDATE billing.reconciliation_state
SET fully_reconciled = false, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND fully_reconciled = true;

-- name: DisarmMerchantEnforcement :exec
-- Clears first-enforce arming, so post-rollback pulls run advisory until an
-- operator re-arms, and trips the merchant's destructive stop, which the intent
-- runner's gate reads: no new provider write starts during the reversal.
INSERT INTO billing.merchant_destructive_policy (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, false, NULL, sqlc.narg(updated_by)::text, sqlc.narg(reason)::text, now())
ON CONFLICT (merchant_id) DO UPDATE SET
    destructive_actions_enabled = false,
    enforce_armed_at = NULL,
    updated_by = EXCLUDED.updated_by,
    reason = EXCLUDED.reason,
    updated_at = now();
