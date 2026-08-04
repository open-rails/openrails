-- or#859 tier 1: the READ side of `openrails undo-run`.
--
-- An undo is dry-run by default, so everything the operator is asked to confirm
-- must be countable without mutating anything. These queries are that plan, and
-- they are deliberately the same predicates the apply path uses — a plan derived
-- from different predicates than the write is a plan that can lie.

-- name: CountPruneRestorableForRun :one
-- What `kind='prune'` would bring back: rows this run tombstoned that are still
-- tombstoned. A row someone already restored by hand is not counted twice.
SELECT
    (SELECT count(*) FROM openrails.subscriptions
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid
        AND destructive_run_id = sqlc.arg(run_id)::uuid
        AND deleted_at IS NOT NULL)::bigint AS subscriptions,
    (SELECT count(*) FROM openrails.payments
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid
        AND destructive_run_id = sqlc.arg(run_id)::uuid
        AND deleted_at IS NOT NULL)::bigint AS payments,
    (SELECT count(*) FROM openrails.checkout_sessions
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid
        AND destructive_run_id = sqlc.arg(run_id)::uuid
        AND deleted_at IS NOT NULL)::bigint AS checkout_sessions,
    (SELECT count(*) FROM openrails.entitlements
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid
        AND destructive_run_id = sqlc.arg(run_id)::uuid
        AND deleted_at IS NOT NULL)::bigint AS entitlements;

-- name: CountConvergeRestorableForRun :one
-- What `kind='converge_enforce'` would re-assert, counted through the SAME join
-- and the SAME `deleted_at IS NULL` guard the restore uses: an image whose row a
-- later prune has since tombstoned belongs to THAT run's reverse, so the plan
-- must not promise it either.
SELECT
    (SELECT count(*)
       FROM openrails.destructive_run_before_images b
       JOIN openrails.subscriptions s
         ON s.merchant_id = b.merchant_id AND s.id = b.row_id
      WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
        AND b.destructive_run_id = sqlc.arg(run_id)::uuid
        AND b.table_name = 'subscriptions'
        AND b.restored_at IS NULL
        AND s.deleted_at IS NULL)::bigint AS subscriptions,
    (SELECT count(*)
       FROM openrails.destructive_run_before_images b
       JOIN openrails.entitlements e
         ON e.merchant_id = b.merchant_id AND e.id = b.row_id
      WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
        AND b.destructive_run_id = sqlc.arg(run_id)::uuid
        AND b.table_name = 'entitlements'
        AND e.deleted_at IS NULL)::bigint AS entitlements_to_invalidate,
    (SELECT count(*)
       FROM openrails.destructive_run_before_images b
       JOIN openrails.subscriptions s
         ON s.merchant_id = b.merchant_id AND s.id = b.row_id
      WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
        AND b.destructive_run_id = sqlc.arg(run_id)::uuid
        AND b.table_name = 'subscriptions'
        AND b.restored_at IS NULL
        AND s.deleted_at IS NOT NULL)::bigint AS subscriptions_tombstoned;

-- name: CountNullPSPBlindSpot :one
-- or#859 §3.2, hole 1: `psp_id` is NULLABLE on all eight PSP-tagged tables, so a
-- PSP-scoped predicate silently SKIPS legacy rows. For a prune that nullability
-- is a safety property; for a rollback it is a coverage hole, and the rule is
-- that the undo reports it as an explicit count rather than excluding it in
-- silence. Live rows only — a tombstoned row is already accounted for by
-- whichever run took it.
SELECT
    (SELECT count(*) FROM openrails.subscriptions
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id IS NULL AND deleted_at IS NULL)::bigint AS subscriptions,
    (SELECT count(*) FROM openrails.payments
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id IS NULL AND deleted_at IS NULL)::bigint AS payments,
    (SELECT count(*) FROM openrails.checkout_sessions
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id IS NULL AND deleted_at IS NULL)::bigint AS checkout_sessions,
    -- payment_methods carries no soft-delete column; every row is live.
    (SELECT count(*) FROM openrails.payment_methods
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id IS NULL)::bigint AS payment_methods,
    (SELECT count(*) FROM openrails.rail_intents
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id IS NULL
        AND status IN ('pending', 'failed_retryable'))::bigint AS unfired_intents;
