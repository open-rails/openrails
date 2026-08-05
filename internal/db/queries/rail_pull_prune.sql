-- #511 pull-provider --prune: account-bound EXCESS detection + or#858 reversible
-- soft deletion.
-- "Excess" = a local row attributed to the pulled psp_id whose
-- provider key is ABSENT from the freshly fetched provider snapshot. or#893
-- made psp_id NOT NULL everywhere, so there is no unattributed lane left to
-- preserve: every row belongs to exactly one PSP and is reachable by exactly
-- one PSP-scoped pass. Matching FAILS CLOSED — a row whose PSP was not pulled
-- is out of scope, never "maybe ours".
--
-- or#858: nothing here DELETEs. A prune sets deleted_at and stamps the row with
-- the prune run that took it, so the whole pass reverses in one step.

-- name: ListExcessSubscriptionsForPSP :many
-- An EMPTY present_ids is not "everything is excess" — it is a snapshot that
-- proved nothing, and `x <> ALL('{}')` is TRUE for every row. The cardinality
-- guard makes an empty remote set match NOTHING here, so even a caller that
-- skipped its own refusal cannot wipe a PSP's book (or#858).
SELECT id FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail_subscription_id <> ''
  AND deleted_at IS NULL
  AND cardinality(sqlc.arg(present_ids)::text[]) > 0
  AND rail_subscription_id <> ALL(sqlc.arg(present_ids)::text[]);

-- name: ListPSPSubscriptionCandidates :many
-- Every account-bound subscription a prune WOULD have considered. Reports what
-- a coverage-blocked pass skipped; never a deletion input.
SELECT id FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail_subscription_id <> ''
  AND deleted_at IS NULL;

-- name: ListExcessPaymentsForPSP :many
-- Windowed: only payments inside the pulled [since, until] window are eligible
-- (a snapshot only proves absence within the window it covered). Same empty-set
-- refusal as the subscription query.
SELECT id FROM openrails.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND deleted_at IS NULL
  AND cardinality(sqlc.arg(present_txns)::text[]) > 0
  AND transaction_id <> ALL(sqlc.arg(present_txns)::text[])
  AND (sqlc.narg(since)::timestamptz IS NULL OR purchased_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR purchased_at <= sqlc.narg(until)::timestamptz);

-- name: ListPSPPaymentCandidates :many
SELECT id FROM openrails.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND deleted_at IS NULL
  AND (sqlc.narg(since)::timestamptz IS NULL OR purchased_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR purchased_at <= sqlc.narg(until)::timestamptz);

-- name: SubscriptionHasGrant :one
-- A subscription that fed the #514 grant ledger is entangled: it must be
-- retracted through convergence (grant revoke), never row-deleted (that would
-- orphan the grant). Such excess subs are surfaced by prune, not deleted.
SELECT EXISTS(
  SELECT 1 FROM openrails.grants
  WHERE merchant_id = sqlc.arg(merchant_id)::uuid
    AND source_type = 'subscription'
    AND source_id = sqlc.arg(subscription_id)::uuid::text
) AS has_grant;

-- name: PaymentHasProtectedDependents :one
-- A payment is unsafe to remove if it feeds the #514 grant ledger, backs a
-- refund, an admin grant, or a checkout session. Such rows are retracted through
-- convergence (grant revoke), never pruned.
SELECT
  EXISTS(SELECT 1 FROM openrails.grants WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_id = sqlc.arg(payment_id)::uuid)
  OR EXISTS(SELECT 1 FROM openrails.payments r WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.refunded_payment_id = sqlc.arg(payment_id)::uuid AND r.deleted_at IS NULL)
  OR EXISTS(SELECT 1 FROM openrails.checkout_sessions cs WHERE cs.merchant_id = sqlc.arg(merchant_id)::uuid AND cs.payment_id = sqlc.arg(payment_id)::uuid AND cs.deleted_at IS NULL)
  AS protected;

-- --- or#858 soft delete ------------------------------------------------------

-- name: PruneSoftDeleteCheckoutSessionsBySubscription :execrows
UPDATE openrails.checkout_sessions
SET deleted_at = sqlc.arg(now)::timestamptz,
    destructive_run_id = sqlc.arg(run_id)::uuid,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND deleted_at IS NULL;

-- name: PruneSoftDeleteEntitlementsBySubscription :execrows
UPDATE openrails.entitlements
SET deleted_at = sqlc.arg(now)::timestamptz,
    destructive_run_id = sqlc.arg(run_id)::uuid,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND source_type = 'subscription'
  AND source_id = sqlc.arg(subscription_id)::uuid
  AND deleted_at IS NULL;

-- name: PruneSoftDeleteSubscriptionByID :execrows
UPDATE openrails.subscriptions
SET deleted_at = sqlc.arg(now)::timestamptz,
    destructive_run_id = sqlc.arg(run_id)::uuid,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(id)::uuid
  AND deleted_at IS NULL;

-- name: PruneSoftDeletePaymentByID :execrows
UPDATE openrails.payments
SET deleted_at = sqlc.arg(now)::timestamptz,
    destructive_run_id = sqlc.arg(run_id)::uuid
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(id)::uuid
  AND deleted_at IS NULL;

-- --- or#858 rollback ---------------------------------------------------------
-- Keyed on the run stamp, so a whole prune reverses as a unit. Restoring only
-- the rows THIS run took means an unrelated soft delete — an ordinary
-- entitlement revocation — is never resurrected by a rollback.

-- name: RestoreSubscriptionsByDestructiveRun :execrows
UPDATE openrails.subscriptions
SET deleted_at = NULL, destructive_run_id = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND destructive_run_id = sqlc.arg(run_id)::uuid;

-- name: RestorePaymentsByDestructiveRun :execrows
UPDATE openrails.payments
SET deleted_at = NULL, destructive_run_id = NULL
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND destructive_run_id = sqlc.arg(run_id)::uuid;

-- name: RestoreCheckoutSessionsByDestructiveRun :execrows
UPDATE openrails.checkout_sessions
SET deleted_at = NULL, destructive_run_id = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND destructive_run_id = sqlc.arg(run_id)::uuid;

-- name: RestoreEntitlementsByDestructiveRun :execrows
UPDATE openrails.entitlements
SET deleted_at = NULL, destructive_run_id = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND destructive_run_id = sqlc.arg(run_id)::uuid;

-- --- or#858 / or#859 destructive-run ledger ----------------------------------
-- Deliberately the GENERAL run table (or#859 §5.1): kind='prune' is its first
-- user. Opened BEFORE anything is written, so a crash mid-run still leaves a
-- reversible record.

-- name: CreateDestructiveRun :one
INSERT INTO openrails.destructive_runs (
    id, merchant_id, psp_id, kind, actor, dry_run, coverage, expected_rows, note
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.narg(psp_id)::uuid,
    sqlc.arg(kind)::text, sqlc.arg(actor)::text, sqlc.arg(dry_run)::boolean,
    sqlc.narg(coverage)::jsonb, sqlc.narg(expected_rows)::bigint, sqlc.narg(note)::text
)
RETURNING *;

-- name: FinishDestructiveRun :one
UPDATE openrails.destructive_runs
SET status = sqlc.arg(status)::text,
    finished_at = sqlc.arg(now)::timestamptz,
    affected = sqlc.narg(affected)::jsonb
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
RETURNING *;

-- name: MarkDestructiveRunReversed :one
UPDATE openrails.destructive_runs
SET status = 'reversed',
    reversed_at = sqlc.arg(now)::timestamptz,
    reversed_by = sqlc.arg(reversed_by)::text
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(id)::uuid
  AND status <> 'reversed'
RETURNING *;

-- name: GetDestructiveRun :one
SELECT * FROM openrails.destructive_runs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListDestructiveRuns :many
SELECT * FROM openrails.destructive_runs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind)::text)
ORDER BY started_at DESC
LIMIT sqlc.arg(lim)::int;
