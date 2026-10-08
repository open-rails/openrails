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

-- Historical archives predate explicit access terms. Rebuild only their derived
-- open projections from the immutable paid grant ledger, as migration 8 does.
-- name: RestoreLegacySubscriptionEntitlementBounds :exec
WITH paid_bounds AS (
    SELECT e.merchant_id, e.id,
           CASE WHEN bool_or(g.ends_at IS NULL) THEN NULL ELSE max(g.ends_at) END AS ends_at
    FROM billing.entitlements e
    JOIN billing.subscriptions s ON s.merchant_id = e.merchant_id AND s.id = e.source_id
    JOIN billing.grants g ON g.merchant_id = e.merchant_id AND g.customer_id = e.customer_id
      AND g.source_type = 'subscription' AND g.source_id = s.id::text
      AND g.kind = 'entitlement' AND g.event = 'grant'
      AND g.spec_snapshot->'entitlements' ? e.entitlement
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid
      AND e.source_type = 'subscription' AND e.ends_at IS NULL
      AND e.revoked_at IS NULL AND e.deleted_at IS NULL
      AND s.access_duration_hours_snapshot IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM billing.grants terminal
          WHERE terminal.merchant_id = g.merchant_id AND terminal.supersedes_id = g.id
            AND terminal.event IN ('revoke', 'expire', 'supersede'))
    GROUP BY e.merchant_id, e.id
)
UPDATE billing.entitlements e SET ends_at = bounds.ends_at
FROM paid_bounds bounds WHERE bounds.merchant_id = e.merchant_id AND bounds.id = e.id
  AND bounds.ends_at IS NOT NULL;
