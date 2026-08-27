-- th-045 OpenRails-owned provider billing evidence and qualification.

-- name: InsertProviderBillingQualification :one
INSERT INTO openrails.provider_billing_qualifications (
    merchant_id,
    operation_id,
    provider,
    provider_resource_id,
    provider_lifetime_start,
    provider_lifetime_end,
    provider_absent_at,
    provider_absence_reference,
    billing_stop_reference,
    windows_closed_at,
    windows_closed_reference,
    lifecycle_evidence_bytes,
    lifecycle_evidence_digest,
    quiescence_seconds
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(operation_id)::text,
    sqlc.arg(provider)::text,
    sqlc.arg(provider_resource_id)::text,
    sqlc.arg(provider_lifetime_start)::timestamptz,
    sqlc.arg(provider_lifetime_end)::timestamptz,
    sqlc.arg(provider_absent_at)::timestamptz,
    sqlc.arg(provider_absence_reference)::text,
    sqlc.arg(billing_stop_reference)::text,
    sqlc.arg(windows_closed_at)::timestamptz,
    sqlc.arg(windows_closed_reference)::text,
    sqlc.arg(lifecycle_evidence_bytes)::bytea,
    sqlc.arg(lifecycle_evidence_digest)::bytea,
    sqlc.arg(quiescence_seconds)::bigint
)
ON CONFLICT (merchant_id, operation_id) DO NOTHING
RETURNING *;

-- name: GetProviderBillingQualification :one
SELECT *
FROM openrails.provider_billing_qualifications
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text;

-- name: GetProviderBillingQualificationForUpdate :one
SELECT *
FROM openrails.provider_billing_qualifications
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
FOR UPDATE;

-- name: InsertProviderBillingObservation :one
INSERT INTO openrails.provider_billing_observations (
    merchant_id,
    operation_id,
    observation_id,
    normalized_query,
    query_start,
    query_end,
    raw_body_available,
    raw_body_bytes,
    raw_body_digest,
    normalized_records_bytes,
    normalized_records_digest,
    provider_cost_usd_micros,
    has_negative_record,
    refusal_kind,
    covers_lifetime,
    qualification_reason,
    observed_at
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(operation_id)::text,
    sqlc.arg(observation_id)::text,
    sqlc.arg(normalized_query)::text,
    sqlc.arg(query_start)::timestamptz,
    sqlc.arg(query_end)::timestamptz,
    sqlc.arg(raw_body_available)::boolean,
    sqlc.arg(raw_body_bytes)::bytea,
    sqlc.arg(raw_body_digest)::bytea,
    sqlc.narg(normalized_records_bytes)::bytea,
    sqlc.narg(normalized_records_digest)::bytea,
    sqlc.narg(provider_cost_usd_micros)::bigint,
    sqlc.arg(has_negative_record)::boolean,
    sqlc.narg(refusal_kind)::text,
    sqlc.arg(covers_lifetime)::boolean,
    sqlc.arg(qualification_reason)::text,
    sqlc.arg(observed_at)::timestamptz
)
ON CONFLICT (merchant_id, operation_id, observation_id) DO NOTHING
RETURNING *;

-- name: GetProviderBillingObservation :one
SELECT *
FROM openrails.provider_billing_observations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
  AND observation_id = sqlc.arg(observation_id)::text;

-- name: UpdateProviderBillingQualification :one
UPDATE openrails.provider_billing_qualifications
SET state = sqlc.arg(state)::text,
    reason = sqlc.arg(reason)::text,
    baseline_observation_id = sqlc.narg(baseline_observation_id)::text,
    qualified_observation_id = sqlc.narg(qualified_observation_id)::text,
    qualified_provider_cost_usd_micros = sqlc.narg(qualified_provider_cost_usd_micros)::bigint,
    qualified_at = sqlc.narg(qualified_at)::timestamptz,
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
RETURNING *;
