-- Operational job state: catalog drift events (reconciliation). Manual rebill
-- attempts were folded into openrails.rail_intents (#358 phase C).

-- name: ListOpenCatalogDriftEvents :many
SELECT * FROM openrails.catalog_drift_events
WHERE merchant_id=openrails.current_merchant_id() AND resolved_at IS NULL;

-- name: UpsertCatalogDriftFinding :one
-- One standing finding per (PSP account, resource, external id, field). An
-- older snapshot never overwrites newer evidence (no row is returned). An
-- operator-ignored identity stays ignored with its recorded values.
INSERT INTO openrails.reconciliation_findings (
    id, merchant_id, finding_type, subject_key, severity, status,
    rail, psp_id, openrails_resource_type, openrails_resource_id,
    external_resource_id, field, openrails_value, external_value,
    created_at, last_seen_at, updated_at
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, 'catalog.' || sqlc.arg(kind)::text,
    jsonb_build_array(sqlc.arg(psp_id)::uuid::text, sqlc.arg(openrails_resource_type)::text,
        coalesce(sqlc.narg(openrails_resource_id)::text,''), coalesce(sqlc.narg(external_resource_id)::text,''),
        coalesce(sqlc.narg(field)::text,''))::text,
    'low', 'reconcile_required', sqlc.arg(rail)::text, sqlc.arg(psp_id)::uuid,
    sqlc.arg(openrails_resource_type)::text, sqlc.narg(openrails_resource_id)::text,
    sqlc.narg(external_resource_id)::text, sqlc.narg(field)::text,
    sqlc.narg(openrails_value)::text, sqlc.narg(external_value)::text,
    sqlc.arg(observed_at)::timestamptz, sqlc.arg(observed_at)::timestamptz, sqlc.arg(observed_at)::timestamptz
)
ON CONFLICT (merchant_id, finding_type, subject_key) DO UPDATE SET
    openrails_value = CASE WHEN openrails.reconciliation_findings.status = 'ignored'
        THEN openrails.reconciliation_findings.openrails_value ELSE EXCLUDED.openrails_value END,
    external_value = CASE WHEN openrails.reconciliation_findings.status = 'ignored'
        THEN openrails.reconciliation_findings.external_value ELSE EXCLUDED.external_value END,
    status = CASE WHEN openrails.reconciliation_findings.status = 'ignored' THEN 'ignored' ELSE 'reconcile_required' END,
    resolved_at = CASE WHEN openrails.reconciliation_findings.status = 'ignored' THEN openrails.reconciliation_findings.resolved_at END,
    resolution = CASE WHEN openrails.reconciliation_findings.status = 'ignored' THEN openrails.reconciliation_findings.resolution END,
    last_seen_at = EXCLUDED.last_seen_at,
    updated_at = EXCLUDED.updated_at
WHERE EXCLUDED.last_seen_at >= openrails.reconciliation_findings.last_seen_at
RETURNING status;

-- name: ResolveCatalogDriftFinding :execrows
-- Absence proof only: the caller has a complete read of this finding's PSP
-- account (or resource) taken no earlier than its latest observation.
UPDATE openrails.reconciliation_findings
SET resolved_at = sqlc.arg(resolved_at)::timestamptz, status = 'fixed', resolution = 'auto_vanished',
    updated_at = sqlc.arg(resolved_at)::timestamptz, notified_at = NULL, notified_severity = NULL
WHERE merchant_id = openrails.current_merchant_id() AND finding_type LIKE 'catalog.%'
  AND id = sqlc.arg(id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND resolved_at IS NULL
  AND last_seen_at <= sqlc.arg(resolved_at)::timestamptz;

-- name: CountOpenCatalogDriftFiltered :one
SELECT count(*) FROM openrails.catalog_drift_events
WHERE merchant_id=openrails.current_merchant_id() AND resolved_at IS NULL
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind)::text)
  AND (sqlc.narg(resource_type)::text IS NULL OR openrails_resource_type = sqlc.narg(resource_type)::text);

-- name: ListOpenCatalogDriftFiltered :many
SELECT * FROM openrails.catalog_drift_events
WHERE merchant_id=openrails.current_merchant_id() AND resolved_at IS NULL
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind)::text)
  AND (sqlc.narg(resource_type)::text IS NULL OR openrails_resource_type = sqlc.narg(resource_type)::text)
ORDER BY detected_at DESC
LIMIT $1::int OFFSET $2::int;

-- name: ResolveCatalogDriftForResource :execrows
-- A per-price reconcile verified this PSP account in sync for the resource.
UPDATE openrails.reconciliation_findings
SET resolved_at = sqlc.arg(resolved_at)::timestamptz, status = 'fixed', resolution = 'enforced',
    updated_at = sqlc.arg(resolved_at)::timestamptz, notified_at = NULL, notified_severity = NULL
WHERE finding_type LIKE 'catalog.%' AND merchant_id = openrails.current_merchant_id() AND resolved_at IS NULL
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND openrails_resource_type = sqlc.arg(openrails_resource_type)::text
  AND openrails_resource_id = sqlc.arg(openrails_resource_id)::text
  AND last_seen_at <= sqlc.arg(resolved_at)::timestamptz;

-- name: CountOpenCatalogDriftByKind :many
SELECT rail, kind, count(*)::bigint AS n
FROM openrails.catalog_drift_events
WHERE merchant_id=openrails.current_merchant_id() AND resolved_at IS NULL
GROUP BY rail, kind;
