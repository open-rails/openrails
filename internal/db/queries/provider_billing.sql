-- th-045 OpenRails-owned provider billing evidence and qualification.

-- name: InsertProviderBillingQualification :one
INSERT INTO billing.cost_qualifications (
    merchant_id,
    operation_id,
    provider,
    provider_resource_id,
    provider_lifetime_starts_at,
    provider_lifetime_ends_at,
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
    sqlc.arg(provider_lifetime_starts_at)::timestamptz,
    sqlc.arg(provider_lifetime_ends_at)::timestamptz,
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

-- name: GetProviderBillingQualificationWithAuthorization :one
SELECT sqlc.embed(q), sqlc.embed(a)
FROM billing.cost_qualifications q
JOIN billing.operation_authorizations a
  ON a.merchant_id = q.merchant_id
 AND a.operation_id = q.operation_id
WHERE q.merchant_id = sqlc.arg(merchant_id)::uuid
  AND q.operation_id = sqlc.arg(operation_id)::text;

-- name: GetProviderBillingQualificationForUpdate :one
SELECT *
FROM billing.cost_qualifications
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
FOR UPDATE;

-- name: InsertProviderBillingObservation :one
INSERT INTO billing.cost_observations (
    merchant_id,
    operation_id,
    observation_id,
    normalized_query,
    query_starts_at,
    query_ends_at,
    raw_body_available,
    raw_body_bytes,
    raw_body_digest,
    normalized_records_bytes,
    normalized_records_digest,
    cost_amount,
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
    sqlc.arg(query_starts_at)::timestamptz,
    sqlc.arg(query_ends_at)::timestamptz,
    sqlc.arg(raw_body_available)::boolean,
    sqlc.arg(raw_body_bytes)::bytea,
    sqlc.arg(raw_body_digest)::bytea,
    sqlc.narg(normalized_records_bytes)::bytea,
    sqlc.narg(normalized_records_digest)::bytea,
    sqlc.narg(cost_amount)::bigint,
    sqlc.arg(has_negative_record)::boolean,
    sqlc.narg(refusal_kind)::text,
    sqlc.arg(covers_lifetime)::boolean,
    sqlc.arg(qualification_reason)::text,
    sqlc.arg(observed_at)::timestamptz
)
ON CONFLICT (merchant_id, operation_id, observation_id) DO NOTHING
RETURNING *;

-- An observation's facts without its bodies: the digests decide a replay and
-- author the settlement, so the stored bytes (up to 768 KB each) are never
-- read back.
-- name: GetProviderBillingObservation :one
SELECT merchant_id, operation_id, observation_id, normalized_query, query_starts_at, query_ends_at,
       raw_body_available, raw_body_digest, normalized_records_digest, cost_amount,
       has_negative_record, refusal_kind, covers_lifetime, qualification_reason, observed_at
FROM billing.cost_observations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
  AND observation_id = sqlc.arg(observation_id)::text;

-- name: UpdateProviderBillingQualification :one
UPDATE billing.cost_qualifications
SET state = sqlc.arg(state)::text,
    reason = sqlc.arg(reason)::text,
    baseline_observation_id = sqlc.narg(baseline_observation_id)::text,
    qualified_observation_id = sqlc.narg(qualified_observation_id)::text,
    qualified_cost_amount = sqlc.narg(qualified_cost_amount)::bigint,
    qualified_at = sqlc.narg(qualified_at)::timestamptz,
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
RETURNING *;

-- An operation's latest observation: once a qualification is refused, the one
-- that refused it, since nothing is appended after.
-- name: GetLatestProviderBillingObservation :one
SELECT merchant_id, operation_id, observation_id, normalized_query, query_starts_at, query_ends_at,
       raw_body_available, raw_body_digest, normalized_records_digest, cost_amount,
       has_negative_record, refusal_kind, covers_lifetime, qualification_reason, observed_at
FROM billing.cost_observations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
ORDER BY observed_at DESC
LIMIT 1;

-- name: InsertProviderBillingResolution :one
INSERT INTO billing.cost_resolutions (
    merchant_id,
    operation_id,
    kind,
    cost_amount,
    attested_by,
    reference,
    note,
    resolved_at
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(operation_id)::text,
    sqlc.arg(kind)::text,
    sqlc.narg(cost_amount)::bigint,
    sqlc.arg(attested_by)::text,
    sqlc.arg(reference)::text,
    sqlc.narg(note)::text,
    sqlc.arg(resolved_at)::timestamptz
)
RETURNING *;

-- name: GetProviderBillingResolution :one
SELECT *
FROM billing.cost_resolutions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text;

-- name: ListProviderBillingResolutions :many
SELECT *
FROM billing.cost_resolutions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = ANY(sqlc.arg(operation_ids)::text[]);

-- Newest first; an empty filter admits every state.
-- name: ListProviderBillingQualifications :many
SELECT sqlc.embed(q), sqlc.embed(a)
FROM billing.cost_qualifications q
JOIN billing.operation_authorizations a
  ON a.merchant_id = q.merchant_id
 AND a.operation_id = q.operation_id
WHERE q.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (cardinality(sqlc.arg(states)::text[]) = 0 OR q.state = ANY(sqlc.arg(states)::text[]))
  AND (cardinality(sqlc.arg(authorization_states)::text[]) = 0 OR a.state = ANY(sqlc.arg(authorization_states)::text[]))
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (q.created_at, q.operation_id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_operation_id)::text))
ORDER BY q.created_at DESC, q.operation_id DESC
LIMIT sqlc.arg(row_limit)::int;
