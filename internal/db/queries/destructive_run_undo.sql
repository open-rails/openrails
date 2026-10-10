-- The read side of `openrails undo-run`: the dry-run plan, counted with the same
-- predicates the apply path uses so the plan cannot disagree with the write.

-- name: CountPruneRestorableForRun :one
-- What `kind='prune'` would bring back: rows this run tombstoned that are still
-- tombstoned. A row someone already restored by hand is not counted twice.
SELECT
    (SELECT count(*) FROM billing.subscriptions
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid
        AND destructive_run_id = sqlc.arg(run_id)::uuid
        AND deleted_at IS NOT NULL)::bigint AS subscriptions,
    (SELECT count(*) FROM billing.payments
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid
        AND destructive_run_id = sqlc.arg(run_id)::uuid
        AND deleted_at IS NOT NULL)::bigint AS payments,
    (SELECT count(*) FROM billing.checkout_attempts
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid
        AND destructive_run_id = sqlc.arg(run_id)::uuid
        AND deleted_at IS NOT NULL)::bigint AS checkout_attempts,
    (SELECT count(*) FROM billing.product_access
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid
        AND destructive_run_id = sqlc.arg(run_id)::uuid
        AND deleted_at IS NOT NULL)::bigint AS product_access;

-- name: CountConvergeRestorableForRun :one
-- What `kind='converge_enforce'` would re-assert, counted through the SAME join
-- and the SAME `deleted_at IS NULL` guard the restore uses: an image whose row a
-- later prune has since tombstoned belongs to THAT run's reverse, so the plan
-- must not promise it either.
SELECT
    (SELECT count(*)
       FROM billing.destructive_run_before_images b
       JOIN billing.subscriptions s
         ON s.merchant_id = b.merchant_id AND s.id = b.row_id
      WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
        AND b.destructive_run_id = sqlc.arg(run_id)::uuid
        AND b.table_name = 'subscriptions'
        AND b.restored_at IS NULL
        AND s.lifecycle_rev = b.after_lifecycle_rev
        AND s.deleted_at IS NULL)::bigint AS subscriptions,
    (SELECT count(*)
       FROM billing.destructive_run_before_images b
       JOIN billing.subscriptions s
         ON s.merchant_id = b.merchant_id AND s.id = b.row_id
      WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
        AND b.destructive_run_id = sqlc.arg(run_id)::uuid
        AND b.table_name = 'subscriptions'
        AND b.restored_at IS NULL
        AND s.lifecycle_rev IS DISTINCT FROM b.after_lifecycle_rev
        AND s.deleted_at IS NULL)::bigint AS subscriptions_changed,
    (SELECT count(*)
       FROM billing.destructive_run_before_images b
       JOIN billing.product_access e
         ON e.merchant_id = b.merchant_id AND e.id = b.row_id
      WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
        AND b.destructive_run_id = sqlc.arg(run_id)::uuid
        AND b.table_name = 'product_access'
        AND e.deleted_at IS NULL)::bigint AS access_to_invalidate,
    (SELECT count(*)
       FROM billing.destructive_run_before_images b
       JOIN billing.subscriptions s
         ON s.merchant_id = b.merchant_id AND s.id = b.row_id
      WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
        AND b.destructive_run_id = sqlc.arg(run_id)::uuid
        AND b.table_name = 'subscriptions'
        AND b.restored_at IS NULL
        AND s.deleted_at IS NOT NULL)::bigint AS subscriptions_tombstoned;

-- name: CountUnattributedProviderRows :one
-- Provider rows lacking PSP provenance. Structurally zero (NOT NULL or CHECK): a
-- non-zero count means the schema changed under the undo, which then refuses.
-- Off-rail (manual) payments carry no PSP by design and are excluded.
SELECT
    (SELECT count(*) FROM billing.subscriptions
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id IS NULL AND deleted_at IS NULL)::bigint AS subscriptions,
    (SELECT count(*) FROM billing.payments
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id IS NULL AND deleted_at IS NULL
        AND channel = 'rail')::bigint AS payments,
    (SELECT count(*) FROM billing.checkout_attempts
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id IS NULL AND deleted_at IS NULL)::bigint AS checkout_attempts,
    -- payment_methods carries no soft-delete column; every row is live. A
    -- card a third-party custodian holds names no PSP: no PSP-scoped
    -- operation reaches it.
    (SELECT count(*) FROM billing.payment_methods
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id IS NULL AND custodian = 'psp')::bigint AS payment_methods,
    -- Custodian-addressed intents name a custodian instead of a PSP
    -- (provider_intents_addressed_check), so they are excluded.
    (SELECT count(*) FROM billing.provider_intents
      WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id IS NULL
        AND custodian_id IS NULL
        AND status IN ('pending', 'failed_retryable'))::bigint AS unfired_intents;
