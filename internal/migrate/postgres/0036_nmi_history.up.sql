-- parent: 35 sha256:5ce2bbaf87989a1799e66287dd37125472d256df9aff777cd0206c9f3232ac94
-- #1120: NMI's own authorization history, as monthly aggregates per NMI PSP:
-- the decline report's numbers, read daily, for the months before OpenRails
-- recorded its own attempts. Never a transaction row. Kept 25 months, like
-- payment attempts.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.nmi_history_months (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    month timestamp with time zone NOT NULL,
    kind text NOT NULL,
    category text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    authorizations bigint NOT NULL,
    CONSTRAINT nmi_history_months_pkey PRIMARY KEY (merchant_id, psp_id, month, kind, category, reason),
    CONSTRAINT nmi_history_months_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES openrails.psps(merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT chk_nmi_history_months_month CHECK (month = date_trunc('month', month, 'UTC')),
    CONSTRAINT chk_nmi_history_months_kind CHECK (kind IN ('verification', 'one_off_sale', 'scheduled_rebill')),
    CONSTRAINT chk_nmi_history_months_category CHECK (category IN ('approved', 'card_data', 'issuer_soft', 'issuer_hard', 'gateway_rule', 'system_error', 'unknown')),
    CONSTRAINT chk_nmi_history_months_reason CHECK ((category = 'approved') = (reason = '')),
    CONSTRAINT chk_nmi_history_months_authorizations CHECK (authorizations > 0)
);

COMMENT ON TABLE openrails.nmi_history_months IS '#1120 authorizations NMI answered per PSP, month (its first instant, UTC), kind (verification, one_off_sale, scheduled_rebill) and outcome: category approved, or a refusal''s category and reason from the one classifier. A read replaces every month it covers.';

CREATE INDEX idx_nmi_history_months_month ON openrails.nmi_history_months USING btree (merchant_id, month);

-- One row per NMI PSP whose history was read: the last read that completed.
CREATE TABLE openrails.nmi_history_reads (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    read_at timestamp with time zone NOT NULL,
    CONSTRAINT nmi_history_reads_pkey PRIMARY KEY (merchant_id, psp_id),
    CONSTRAINT nmi_history_reads_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES openrails.psps(merchant_id, id) ON DELETE CASCADE
);

COMMENT ON TABLE openrails.nmi_history_reads IS '#1120 when each NMI PSP''s history was last read in full. A PSP with none is backfilled 25 months; later reads start the month before this one.';

-- The cleanup worker's work queue also lists merchants holding NMI history
-- months past the attempt retention.
CREATE OR REPLACE FUNCTION openrails.retention_work_merchant_ids(p_now timestamp with time zone, p_notification_cutoff timestamp with time zone, p_notification_seen_cutoff timestamp with time zone, p_webhook_cutoff timestamp with time zone, p_settlement_cutoff timestamp with time zone, p_lifecycle_cutoff timestamp with time zone, p_attempt_cutoff timestamp with time zone, p_after uuid, p_limit integer) RETURNS TABLE(merchant_id uuid)
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
            UNION
            (SELECT DISTINCT nh.merchant_id AS mid
               FROM openrails.nmi_history_months nh
              WHERE (p_after IS NULL OR nh.merchant_id > p_after)
                AND nh.month < p_attempt_cutoff
              ORDER BY 1 LIMIT p_limit)
           ) q
     ORDER BY q.mid
     LIMIT p_limit;
END;
$$;
COMMENT ON FUNCTION openrails.retention_work_merchant_ids(p_now timestamp with time zone, p_notification_cutoff timestamp with time zone, p_notification_seen_cutoff timestamp with time zone, p_webhook_cutoff timestamp with time zone, p_settlement_cutoff timestamp with time zone, p_lifecycle_cutoff timestamp with time zone, p_attempt_cutoff timestamp with time zone, p_after uuid, p_limit integer) IS 'or#837: merchants with retention work: an expirable checkout session past its TTL, a notification/webhook-dedup row past its window, an ACKED settlement/host-lifecycle event past its prune age, or a payment attempt/rebill cycle/NMI history month past its retention (#1118, #1120). The fan-out list for CleanupExpiredDataWorker; ids only, after a cursor, capped; the deletes run per-merchant in bounded batches.';
