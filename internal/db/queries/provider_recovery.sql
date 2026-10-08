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
  SELECT COALESCE(i.period_starts_at, NULLIF(pi.payload->>'accepted_at','')::timestamptz, NULLIF(pi.result_evidence->>'submitted_at','')::timestamptz, pi.created_at)
    FROM billing.provider_intents pi LEFT JOIN billing.invoices i ON i.merchant_id=pi.merchant_id AND i.id::text=pi.payload->>'invoice_id'
    WHERE pi.merchant_id=sqlc.arg(merchant_id)::uuid AND pi.psp_id=sqlc.arg(psp_id)::uuid
      AND pi.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')
  UNION ALL
  SELECT i.period_starts_at FROM billing.invoices i
    JOIN billing.payment_methods pm ON pm.merchant_id=i.merchant_id AND pm.customer_id=i.customer_id
    WHERE i.merchant_id=sqlc.arg(merchant_id)::uuid AND pm.psp_id=sqlc.arg(psp_id)::uuid
      AND i.status IN ('draft','open','past_due','uncollectible')
) facts;

-- name: PSPRecoveryBookAge :one
-- Semantic obligation dates survive process clocks and invoice-only books.
-- A recent additional fact cannot hide older state; impossible future evidence
-- is not permission to treat an inherited book as new.
SELECT COALESCE(bool_or(at < sqlc.arg(before)::timestamptz), false)::boolean AS established,
       COALESCE(bool_or(at > sqlc.arg(latest)::timestamptz), false)::boolean AS future
FROM (
  SELECT started_at AS at FROM billing.subscriptions
    WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
  UNION ALL
  SELECT purchased_at FROM billing.payments
    WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
  UNION ALL
  SELECT COALESCE(i.period_starts_at, NULLIF(pi.payload->>'accepted_at','')::timestamptz, NULLIF(pi.result_evidence->>'submitted_at','')::timestamptz, pi.created_at)
    FROM billing.provider_intents pi LEFT JOIN billing.invoices i ON i.merchant_id=pi.merchant_id AND i.id::text=pi.payload->>'invoice_id'
    WHERE pi.merchant_id=sqlc.arg(merchant_id)::uuid AND pi.psp_id=sqlc.arg(psp_id)::uuid
      AND pi.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')
  UNION ALL
  SELECT i.period_starts_at FROM billing.invoices i
    JOIN billing.payment_methods pm ON pm.merchant_id=i.merchant_id AND pm.customer_id=i.customer_id
    WHERE i.merchant_id=sqlc.arg(merchant_id)::uuid AND pm.psp_id=sqlc.arg(psp_id)::uuid
      AND i.status IN ('draft','open','past_due','uncollectible')
) facts;
