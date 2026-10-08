-- name: GetPSPAppliedRefreshWatermark :one
SELECT watermark_at FROM billing.psp_refresh_watermarks
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid AND event_domain = 'applied_events';

-- name: UpsertPSPAppliedRefreshWatermark :exec
INSERT INTO billing.psp_refresh_watermarks (merchant_id, psp_id, event_domain, watermark_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, 'applied_events', sqlc.arg(watermark_at)::timestamptz)
ON CONFLICT (merchant_id, psp_id, event_domain) DO UPDATE
SET watermark_at = GREATEST(billing.psp_refresh_watermarks.watermark_at, EXCLUDED.watermark_at), updated_at = now();

-- name: PSPRecoveryHistoryFloor :one
-- Only live obligations and unresolved accepted operations require recovery.
-- Historical completed payments do not make a healthy current book scan years.
SELECT COALESCE(min(at), sqlc.arg(fallback)::timestamptz)::timestamptz AS oldest_at FROM (
  SELECT COALESCE(current_period_starts_at, started_at) AS at FROM billing.subscriptions
    WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
      AND status IN ('active','past_due','awaiting_method','unverified') AND deleted_at IS NULL
  UNION ALL
  SELECT created_at FROM billing.provider_intents
    WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
      AND status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')
) facts;
