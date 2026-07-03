-- Operational job state: catalog drift events (reconciliation). Manual rebill
-- attempts were folded into openrails.rail_intents (#358 phase C).

-- name: ListOpenCatalogDriftEvents :many
SELECT * FROM openrails.catalog_drift_events
WHERE resolved_at IS NULL;

-- name: InsertCatalogDriftEvent :exec
INSERT INTO openrails.catalog_drift_events (
    id, merchant_id, rail, kind, openrails_resource_type, openrails_resource_id,
    external_resource_id, field, openrails_value, external_value, detected_at
) VALUES ($1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ResolveCatalogDriftEvent :exec
UPDATE openrails.catalog_drift_events
SET resolved_at = $2
WHERE id = $1 AND resolved_at IS NULL;

-- name: CountOpenCatalogDriftFiltered :one
SELECT count(*) FROM openrails.catalog_drift_events
WHERE resolved_at IS NULL
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind)::text)
  AND (sqlc.narg(resource_type)::text IS NULL OR openrails_resource_type = sqlc.narg(resource_type)::text);

-- name: ListOpenCatalogDriftFiltered :many
SELECT * FROM openrails.catalog_drift_events
WHERE resolved_at IS NULL
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind)::text)
  AND (sqlc.narg(resource_type)::text IS NULL OR openrails_resource_type = sqlc.narg(resource_type)::text)
ORDER BY detected_at DESC
LIMIT $1::int OFFSET $2::int;

-- name: ResolveCatalogDriftForResource :execrows
UPDATE openrails.catalog_drift_events
SET resolved_at = $1
WHERE resolved_at IS NULL
  AND openrails_resource_type = $2
  AND openrails_resource_id = $3;

-- name: CountOpenCatalogDriftByKind :many
SELECT rail, kind, count(*)::bigint AS n
FROM openrails.catalog_drift_events
WHERE resolved_at IS NULL
GROUP BY rail, kind;
