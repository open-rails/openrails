-- Merchant billing archive export/restore (the per-table SQL is profile-driven
-- and stays in internal/merchantarchive).

-- name: LiveMerchantExists :one
SELECT EXISTS (
    SELECT 1 FROM billing.merchants
    WHERE id = sqlc.arg(id)::uuid AND status = 'active' AND deleted_at IS NULL
);

-- name: CheckBillingRestoreLedger :exec
SELECT billing.check_billing_restore_ledger(sqlc.arg(merchant_id)::uuid);

-- name: BeginBillingRestore :one
SELECT billing.begin_billing_restore(sqlc.arg(merchant_id)::uuid);

-- name: FinishBillingRestore :exec
SELECT billing.finish_billing_restore(sqlc.arg(merchant_id)::uuid, sqlc.arg(digest)::text, sqlc.arg(rows)::bigint);

-- CASE keeps rows nullable for sqlc (a fresh run has no summary).
-- name: GetBillingRestoreReceipt :one
SELECT summary ->> 'digest' AS digest, CASE WHEN summary ? 'rows' THEN (summary ->> 'rows')::bigint END AS rows
FROM billing.maintenance_runs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: SetCatalogRevision :exec
UPDATE billing.merchants SET catalog_revision = sqlc.arg(revision)::bigint WHERE id = sqlc.arg(merchant_id)::uuid;

-- name: CatalogApplicationsAfterRevisionExist :one
SELECT EXISTS (
    SELECT 1 FROM billing.catalog_applications
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND applied_revision > sqlc.arg(revision)::bigint
);
