-- pull-provider --prune: excess detection and reversible soft deletion. Excess =
-- a local row of the pulled psp_id whose provider key is absent from the fresh
-- snapshot. Every row belongs to exactly one PSP and matching fails closed: a
-- row whose PSP was not pulled is out of scope. Nothing here DELETEs: a prune
-- sets deleted_at and stamps the run, so the pass reverses in one step.

-- name: ListExcessSubscriptionsForPSP :many
-- An empty present_ids proves nothing, yet `x <> ALL('{}')` is true for every
-- row: the cardinality guard makes it match nothing, so even a caller that
-- skipped its own refusal cannot wipe a PSP's book.
SELECT id FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail_subscription_id IS NOT NULL
  AND deleted_at IS NULL
  AND cardinality(sqlc.arg(present_ids)::text[]) > 0
  AND rail_subscription_id <> ALL(sqlc.arg(present_ids)::text[]);

-- name: ListPSPSubscriptionCandidates :many
-- Every account-bound subscription a prune WOULD have considered. Reports what
-- a coverage-blocked pass skipped; never a deletion input.
SELECT id FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail_subscription_id IS NOT NULL
  AND deleted_at IS NULL;

-- name: ListExcessPaymentsForPSP :many
-- Windowed: only payments inside the pulled [since, until] window are eligible
-- (a snapshot only proves absence within the window it covered). Same empty-set
-- refusal as the subscription query.
SELECT id FROM billing.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND deleted_at IS NULL
  AND cardinality(sqlc.arg(present_txns)::text[]) > 0
  AND transaction_id <> ALL(sqlc.arg(present_txns)::text[])
  AND (sqlc.narg(since)::timestamptz IS NULL OR purchased_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR purchased_at <= sqlc.narg(until)::timestamptz);

-- name: ListPSPPaymentCandidates :many
SELECT id FROM billing.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND deleted_at IS NULL
  AND (sqlc.narg(since)::timestamptz IS NULL OR purchased_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR purchased_at <= sqlc.narg(until)::timestamptz);

-- name: SubscriptionHasGrant :one
-- A subscription that fed the grant ledger is retracted through convergence
-- (grant revoke), never row-deleted, which would orphan the grant: prune only
-- surfaces it.
SELECT EXISTS(
  SELECT 1 FROM billing.grants
  WHERE merchant_id = sqlc.arg(merchant_id)::uuid
    AND source_type = 'subscription'
    AND source_id = sqlc.arg(subscription_id)::uuid::text
) AS has_grant;

-- name: PaymentHasProtectedDependents :one
-- A payment that feeds a grant or backs a refund or checkout attempt is
-- retracted through convergence (grant revoke), never pruned.
SELECT
  EXISTS(SELECT 1 FROM billing.grants WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_id = sqlc.arg(payment_id)::uuid)
  OR EXISTS(SELECT 1 FROM billing.payments r WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.refunded_payment_id = sqlc.arg(payment_id)::uuid AND r.deleted_at IS NULL)
  OR EXISTS(SELECT 1 FROM billing.checkout_attempts cs WHERE cs.merchant_id = sqlc.arg(merchant_id)::uuid AND cs.payment_id = sqlc.arg(payment_id)::uuid AND cs.deleted_at IS NULL)
  AS protected;

-- name: PruneSoftDeleteCheckoutAttemptsBySubscription :execrows
UPDATE billing.checkout_attempts
SET deleted_at = sqlc.arg(now)::timestamptz,
    destructive_run_id = sqlc.arg(run_id)::uuid,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND deleted_at IS NULL;

-- name: PruneSoftDeleteAccessBySubscription :execrows
UPDATE billing.product_access
SET deleted_at = sqlc.arg(now)::timestamptz,
    destructive_run_id = sqlc.arg(run_id)::uuid,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND source_type IN ('subscription', 'grace')
  AND source_id = sqlc.arg(subscription_id)::uuid::text
  AND deleted_at IS NULL;

-- name: PruneSoftDeleteSubscriptionByID :execrows
UPDATE billing.subscriptions
SET deleted_at = sqlc.arg(now)::timestamptz,
    destructive_run_id = sqlc.arg(run_id)::uuid,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid

  AND id = sqlc.arg(id)::uuid
  AND deleted_at IS NULL;

-- name: PruneSoftDeletePaymentByID :execrows
UPDATE billing.payments
SET deleted_at = sqlc.arg(now)::timestamptz,
    destructive_run_id = sqlc.arg(run_id)::uuid
WHERE merchant_id = sqlc.arg(merchant_id)::uuid

  AND id = sqlc.arg(id)::uuid
  AND deleted_at IS NULL;

-- Rollback is keyed on the run stamp: a whole prune reverses as a unit, and an
-- unrelated soft delete is never resurrected.

-- name: RestoreSubscriptionsByDestructiveRun :execrows
UPDATE billing.subscriptions
SET deleted_at = NULL, destructive_run_id = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND destructive_run_id = sqlc.arg(run_id)::uuid;

-- name: RestorePaymentsByDestructiveRun :execrows
UPDATE billing.payments
SET deleted_at = NULL, destructive_run_id = NULL
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND destructive_run_id = sqlc.arg(run_id)::uuid;

-- name: RestoreCheckoutAttemptsByDestructiveRun :execrows
UPDATE billing.checkout_attempts
SET deleted_at = NULL, destructive_run_id = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND destructive_run_id = sqlc.arg(run_id)::uuid;

-- name: RestoreAccessByDestructiveRun :execrows
UPDATE billing.product_access
SET deleted_at = NULL, destructive_run_id = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND destructive_run_id = sqlc.arg(run_id)::uuid;

-- Destructive runs (maintenance_runs kinds prune, converge_enforce,
-- merchant_purge). A run is opened before anything is written, so a crash
-- mid-run still leaves a reversible record.

-- name: CreateDestructiveRun :one
INSERT INTO billing.maintenance_runs (
    id, merchant_id, psp_id, kind, actor, dry_run, coverage, expected_rows, note
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.narg(psp_id)::uuid,
    sqlc.arg(kind)::text, sqlc.arg(actor)::text, sqlc.arg(dry_run)::boolean,
    sqlc.narg(coverage)::jsonb, sqlc.narg(expected_rows)::bigint, sqlc.narg(note)::text
)
RETURNING *;

-- name: FinishDestructiveRun :one
UPDATE billing.maintenance_runs
SET status = sqlc.arg(status)::text,
    finished_at = sqlc.arg(now)::timestamptz,
    affected = sqlc.narg(affected)::jsonb
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND kind IN ('prune','converge_enforce','merchant_purge') AND id = sqlc.arg(id)::uuid
RETURNING *;

-- name: MarkDestructiveRunReversed :one
UPDATE billing.maintenance_runs
SET status = 'reversed',
    reversed_at = sqlc.arg(now)::timestamptz,
    reversed_by = sqlc.arg(reversed_by)::text
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND kind IN ('prune','converge_enforce','merchant_purge')
  AND id = sqlc.arg(id)::uuid
  AND status <> 'reversed'
RETURNING *;

-- name: GetDestructiveRun :one
SELECT * FROM billing.maintenance_runs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND kind IN ('prune','converge_enforce','merchant_purge') AND id = sqlc.arg(id)::uuid;

-- name: ListDestructiveRuns :many
SELECT * FROM billing.maintenance_runs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND kind IN ('prune','converge_enforce','merchant_purge')
  AND (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind)::text)
ORDER BY started_at DESC
LIMIT sqlc.arg(lim)::int;
