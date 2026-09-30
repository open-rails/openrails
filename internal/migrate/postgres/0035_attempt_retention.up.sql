-- parent: 34 sha256:8bcb4d9265e461b1a0cc0f92287b0fa8abf90ef726bdab853f3f096e09e88d03
-- #1118: payment attempts and rebill cycles are kept 25 months. The cleanup
-- worker's work queue also lists merchants holding either past that cutoff.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

DROP FUNCTION openrails.retention_work_merchant_ids(p_now timestamp with time zone, p_notification_cutoff timestamp with time zone, p_notification_seen_cutoff timestamp with time zone, p_webhook_cutoff timestamp with time zone, p_settlement_cutoff timestamp with time zone, p_lifecycle_cutoff timestamp with time zone, p_after uuid, p_limit integer);

CREATE FUNCTION openrails.retention_work_merchant_ids(p_now timestamp with time zone, p_notification_cutoff timestamp with time zone, p_notification_seen_cutoff timestamp with time zone, p_webhook_cutoff timestamp with time zone, p_settlement_cutoff timestamp with time zone, p_lifecycle_cutoff timestamp with time zone, p_attempt_cutoff timestamp with time zone, p_after uuid, p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT q.mid
      FROM (
            (SELECT DISTINCT cs.merchant_id AS mid
               FROM openrails.checkout_sessions cs
              WHERE (p_after IS NULL OR cs.merchant_id > p_after)
                AND cs.expires_at IS NOT NULL AND cs.expires_at < p_now
                AND cs.deleted_at IS NULL
                AND cs.status IN ('created', 'requires_action')
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT nq.merchant_id AS mid
               FROM openrails.notifications nq
              WHERE (p_after IS NULL OR nq.merchant_id > p_after)
                AND (nq.created_at < p_notification_cutoff
                     OR (nq.read_at IS NOT NULL AND nq.created_at < p_notification_seen_cutoff))
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT we.merchant_id AS mid
               FROM openrails.webhook_events we
              WHERE (p_after IS NULL OR we.merchant_id > p_after)
                AND we.completed_at < p_webhook_cutoff
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT pse.merchant_id AS mid
               FROM openrails.host_outbox pse
              WHERE (p_after IS NULL OR pse.merchant_id > p_after)
                AND pse.event_type = 'payment.settled' AND pse.delivered_at IS NOT NULL
                AND pse.delivered_at < p_settlement_cutoff
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT hle.merchant_id AS mid
               FROM openrails.host_outbox hle
              WHERE (p_after IS NULL OR hle.merchant_id > p_after)
                AND hle.event_type <> 'payment.settled' AND hle.delivered_at IS NOT NULL
                AND hle.delivered_at < p_lifecycle_cutoff
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT pa.merchant_id AS mid
               FROM openrails.payment_attempts pa
              WHERE (p_after IS NULL OR pa.merchant_id > p_after)
                AND pa.attempted_at < p_attempt_cutoff
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT rc.merchant_id AS mid
               FROM openrails.rebill_cycles rc
              WHERE (p_after IS NULL OR rc.merchant_id > p_after)
                AND rc.due_at < p_attempt_cutoff
              ORDER BY 1 LIMIT p_limit)
           ) q
     ORDER BY q.mid
     LIMIT p_limit;
END;
$$;
COMMENT ON FUNCTION openrails.retention_work_merchant_ids(p_now timestamp with time zone, p_notification_cutoff timestamp with time zone, p_notification_seen_cutoff timestamp with time zone, p_webhook_cutoff timestamp with time zone, p_settlement_cutoff timestamp with time zone, p_lifecycle_cutoff timestamp with time zone, p_attempt_cutoff timestamp with time zone, p_after uuid, p_limit integer) IS 'or#837: merchants with retention work: an expirable checkout session past its TTL, a notification/webhook-dedup row past its window, an ACKED settlement/host-lifecycle event past its prune age, or a payment attempt/rebill cycle past its retention (#1118). The fan-out list for CleanupExpiredDataWorker; ids only, after a cursor, capped; the deletes run per-merchant in bounded batches.';

REVOKE ALL ON FUNCTION openrails.retention_work_merchant_ids(p_now timestamp with time zone, p_notification_cutoff timestamp with time zone, p_notification_seen_cutoff timestamp with time zone, p_webhook_cutoff timestamp with time zone, p_settlement_cutoff timestamp with time zone, p_lifecycle_cutoff timestamp with time zone, p_attempt_cutoff timestamp with time zone, p_after uuid, p_limit integer) FROM PUBLIC;
