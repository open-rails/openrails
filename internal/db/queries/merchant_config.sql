-- Merchant configuration lives in a file or Vault; these are the Postgres
-- side effects of serving it.

-- name: NotifyMerchantConfig :exec
-- Tells every replica to reload one merchant's configuration.
SELECT pg_notify(sqlc.arg(channel)::text, sqlc.arg(payload)::text);

-- name: RearmConfigFindingNotifications :many
-- Open configuration findings of the type last notified before
-- notified_before: their notification re-arms, so they alert again while
-- unresolved.
UPDATE billing.reconciliation_findings
SET notified_at = NULL, notified_severity = NULL, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text
  AND psp_id IS NULL AND resolved_at IS NULL AND status = 'requires_review'
  AND notified_at < sqlc.arg(notified_before)::timestamptz
RETURNING *;

-- name: ResolveConfigFinding :execrows
-- Closes the open configuration finding of a merchant once its cause is gone.
UPDATE billing.reconciliation_findings
SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text
  AND subject_key = sqlc.arg(subject_key)::text AND psp_id IS NULL AND resolved_at IS NULL;
