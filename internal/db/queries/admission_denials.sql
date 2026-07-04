-- openrails.admission_denials_hourly — #733 aggregated admission-denial
-- counters, flushed periodically from Redis (never per-request).

-- name: UpsertAdmissionDenials :exec
INSERT INTO openrails.admission_denials_hourly (merchant_id, customer_id, denial_reason, hour_at, denials, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (merchant_id, customer_id, denial_reason, hour_at)
DO UPDATE SET denials = openrails.admission_denials_hourly.denials + EXCLUDED.denials, updated_at = now();
