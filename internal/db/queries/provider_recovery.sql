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
    FROM billing.provider_intents pi LEFT JOIN billing.orders i ON i.merchant_id=pi.merchant_id AND i.id::text=COALESCE(pi.payload->>'order_id', pi.payload->>'invoice_id')
    WHERE pi.merchant_id=sqlc.arg(merchant_id)::uuid AND pi.psp_id=sqlc.arg(psp_id)::uuid
      AND pi.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')
  UNION ALL
  -- An unbound unpaid usage order may have charged on any known merchant
  -- account after the restored snapshot. Looking only at its newly selected
  -- card is not evidence that the old order was unpaid elsewhere.
  SELECT i.period_starts_at FROM billing.orders i
    WHERE i.merchant_id=sqlc.arg(merchant_id)::uuid AND i.origin='usage'
      AND i.status IN ('open','uncollectible')
      AND EXISTS (SELECT 1 FROM billing.psps p WHERE p.merchant_id=i.merchant_id AND p.id=sqlc.arg(psp_id)::uuid AND p.rail IN ('nmi','stripe'))
) facts;

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
-- Ignore silences an operator notification; it does not settle a receipt. A
-- provider record resolved as foreign is settled: Foreign proved at resolution
-- that nothing of the merchant names it (ProposalSubjectIsLocal); a non-live
-- PSP's unmatched object is settled when raised (evidence.quiet_foreign).
SELECT EXISTS(SELECT 1 FROM billing.reconciliation_findings f
 WHERE f.merchant_id=sqlc.arg(merchant_id)::uuid AND f.psp_id=sqlc.arg(psp_id)::uuid
   AND f.status IN ('reconcile_required','requires_review','ignored')
   AND f.finding_type IN ('pull.charge.missing','pull.refund.missing','pull.reversal.unlinked',
     'pull.dispute.chargeback','pull.subscription.missing','pull.subscription.duplicate','pull.subscription.drift',
     'pull.subscription.facts')
   AND f.resolution IS DISTINCT FROM 'foreign'
   AND NOT COALESCE(f.evidence @> '{"quiet_foreign":true}', false))::boolean;

