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
  -- An unbound unpaid invoice may have charged on any known merchant account
  -- after the restored snapshot. Looking only at its newly selected card is
  -- not evidence that the old invoice was unpaid elsewhere.
  SELECT i.period_starts_at FROM billing.invoices i
    WHERE i.merchant_id=sqlc.arg(merchant_id)::uuid AND i.collection_intent_id IS NULL
      AND i.status IN ('draft','open','past_due','uncollectible')
      AND EXISTS (SELECT 1 FROM billing.psps p WHERE p.merchant_id=i.merchant_id AND p.id=sqlc.arg(psp_id)::uuid AND p.rail IN ('nmi','stripe'))
) facts;

-- name: PSPRecoveryBookAge :one
-- This cold-path exemption uses EXISTS: one older bound fact is sufficient.
-- Fresh completed coverage is checked first; ordinary writes do not scan history.
SELECT EXISTS (
  SELECT 1 FROM billing.subscriptions
    WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
      AND started_at < sqlc.arg(before)::timestamptz
  UNION ALL
  SELECT 1 FROM billing.payments
    WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
      AND purchased_at < sqlc.arg(before)::timestamptz
  UNION ALL
  SELECT 1 FROM billing.invoice_payments ip
    JOIN billing.invoices i ON i.merchant_id=ip.merchant_id AND i.id=ip.invoice_id
    WHERE ip.merchant_id=sqlc.arg(merchant_id)::uuid AND ip.psp_id=sqlc.arg(psp_id)::uuid
      AND i.period_starts_at < sqlc.arg(before)::timestamptz
  UNION ALL
  SELECT 1 FROM billing.provider_intents pi
    LEFT JOIN billing.invoices i ON i.merchant_id=pi.merchant_id AND i.id::text=pi.payload->>'invoice_id'
    WHERE pi.merchant_id=sqlc.arg(merchant_id)::uuid AND pi.psp_id=sqlc.arg(psp_id)::uuid
      AND pi.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')
      AND COALESCE(i.period_starts_at, NULLIF(pi.payload->>'accepted_at','')::timestamptz,
        NULLIF(pi.result_evidence->>'submitted_at','')::timestamptz, pi.created_at) < sqlc.arg(before)::timestamptz
  UNION ALL
  SELECT 1 FROM billing.invoices i
    WHERE i.merchant_id=sqlc.arg(merchant_id)::uuid AND i.collection_intent_id IS NULL
      AND i.status IN ('draft','open','past_due','uncollectible')
      AND i.period_starts_at < sqlc.arg(before)::timestamptz
      AND EXISTS (SELECT 1 FROM billing.psps p WHERE p.merchant_id=i.merchant_id AND p.id=sqlc.arg(psp_id)::uuid AND p.rail IN ('nmi','stripe'))
)::boolean;

-- name: GetPSPCompletedRefreshWatermark :one
SELECT watermark_at FROM billing.psp_refresh_watermarks
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
  AND event_domain='completed_events';

-- name: UpsertPSPCompletedRefreshWatermark :execrows
INSERT INTO billing.psp_refresh_watermarks(merchant_id,psp_id,event_domain,watermark_at)
VALUES(sqlc.arg(merchant_id)::uuid,sqlc.arg(psp_id)::uuid,'completed_events',sqlc.arg(watermark_at)::timestamptz)
ON CONFLICT(merchant_id,psp_id,event_domain) DO UPDATE
SET watermark_at=EXCLUDED.watermark_at,updated_at=now()
WHERE billing.psp_refresh_watermarks.watermark_at<EXCLUDED.watermark_at;

-- name: PSPHasUnresolvedFinancialFindings :one
-- Ignore silences an operator notification; it does not settle a receipt.
SELECT EXISTS(SELECT 1 FROM billing.reconciliation_findings
 WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
   AND status IN ('reconcile_required','requires_review','ignored')
   AND finding_type IN ('pull.charge.missing','pull.refund.missing','pull.reversal.unlinked',
     'pull.dispute.chargeback','pull.subscription.missing','pull.subscription.duplicate','pull.subscription.drift'))::boolean;

-- name: ResumeProviderRecoveryHeldOperations :many
-- Only recovery delays are expedited; issuer retry dates and live leases stand.
UPDATE billing.provider_intents
SET next_attempt_at=sqlc.arg(now)::timestamptz,
    result_evidence=result_evidence-'recovery_held', updated_at=now()
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id IN (
 SELECT id FROM billing.provider_intents
 WHERE merchant_id=sqlc.arg(merchant_id)::uuid
   AND (sqlc.narg(intent_id)::uuid IS NULL OR id=sqlc.narg(intent_id)::uuid)
   AND status IN ('pending','failed_retryable','unknown_needs_verify')
   AND result_evidence @> '{"recovery_held":true}'::jsonb
   AND (lease_expires_at IS NULL OR lease_expires_at<=sqlc.arg(now)::timestamptz)
 ORDER BY id LIMIT sqlc.arg(batch_size)::int
 FOR UPDATE SKIP LOCKED
)
RETURNING id;
