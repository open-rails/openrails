-- billing.admission_denials_hourly: admission-denial counters, flushed from
-- Redis once each hour closes (never per-request).

-- name: UpsertAdmissionDenials :exec
-- A closed hour's count as it stands: a repeated flush writes the same value.
INSERT INTO billing.admission_denials_hourly (merchant_id, customer_id, denial_reason, hour_at, denials, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (merchant_id, customer_id, denial_reason, hour_at)
DO UPDATE SET denials = EXCLUDED.denials, updated_at = now();
