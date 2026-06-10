-- Operational job state: manual rebill attempts (dunning) and catalog drift
-- events (reconciliation).

-- name: GetManualRebillAttemptForUpdate :one
SELECT * FROM billing.manual_rebill_attempts
WHERE subscription_id = $1 AND period_end = $2
FOR UPDATE;

-- name: InsertManualRebillAttempt :exec
INSERT INTO billing.manual_rebill_attempts (
    id, subscription_id, period_end, processor, order_reference,
    status, transaction_id, failure_reason, claimed_until, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: UpdateManualRebillAttempt :exec
UPDATE billing.manual_rebill_attempts
SET status = $2,
    transaction_id = $3,
    failure_reason = $4,
    claimed_until = $5,
    updated_at = $6
WHERE id = $1;

-- name: CountManualRebillAttempts :one
SELECT count(*) FROM billing.manual_rebill_attempts mra
WHERE (sqlc.narg(status)::text IS NULL OR mra.status = sqlc.narg(status)::text)
  AND (sqlc.narg(processor)::text IS NULL OR mra.processor = sqlc.narg(processor)::text);

-- name: ListManualRebillAttempts :many
SELECT * FROM billing.manual_rebill_attempts mra
WHERE (sqlc.narg(status)::text IS NULL OR mra.status = sqlc.narg(status)::text)
  AND (sqlc.narg(processor)::text IS NULL OR mra.processor = sqlc.narg(processor)::text)
ORDER BY mra.updated_at DESC
LIMIT $1::int OFFSET $2::int;

-- name: ListOpenCatalogDriftEvents :many
SELECT * FROM billing.catalog_drift_events
WHERE resolved_at IS NULL;

-- name: InsertCatalogDriftEvent :exec
INSERT INTO billing.catalog_drift_events (
    id, provider, kind, openrails_resource_type, openrails_resource_id,
    external_resource_id, field, openrails_value, external_value, detected_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ResolveCatalogDriftEvent :exec
UPDATE billing.catalog_drift_events
SET resolved_at = $2
WHERE id = $1 AND resolved_at IS NULL;
