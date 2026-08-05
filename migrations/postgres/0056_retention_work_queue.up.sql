-- or#837: the hourly retention sweep must cost ACTIVITY, not the merchant directory.
--
-- CleanupExpiredDataWorker walked `ListActiveMerchantIDs` — every active merchant,
-- every hour, ORDER BY id, no cursor and no LIMIT — and then ran five retention
-- statements for each one. At thousands of merchants that is thousands of empty
-- transactions an hour to find the handful that actually have rows past a cutoff.
-- A bare LIMIT would have been worse than nothing: with a stable ORDER BY id it
-- would sweep the same head forever and starve the tail.
--
-- Two pieces, same shape as 0021/0022/0023:
--   * a SECURITY DEFINER work queue returning ONLY merchants that hold at least
--     one row past one of the retention cutoffs, after a cursor, capped;
--   * a durable cursor so a capped pass resumes at the next merchant instead of
--     re-serving the head.
--
-- The rows themselves are still deleted per-merchant inside RunInMerchantScope,
-- under that merchant's own policy. The queue returns ids only.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- --------------------------------------------------------------------------
-- Index coverage for the queue legs (and the batched deletes that follow)
-- --------------------------------------------------------------------------

-- Every leg is `merchant_id > cursor AND <age column> < cutoff`, ordered by
-- merchant_id and stopped at the merchant cap, so each leg reads at most the
-- capped merchants' due rows — never the whole table. The SAME indexes serve
-- the per-merchant batched DELETEs, which is why they lead with merchant_id.
--
-- payment_settlement_events and host_lifecycle_events already have theirs
-- (idx_payment_settlement_events_delivered, ix_host_lifecycle_events_delivered).

CREATE INDEX ix_notification_queue_retention
    ON openrails.notification_queue USING btree (merchant_id, created_at);

CREATE INDEX ix_webhook_events_retention
    ON openrails.webhook_events USING btree (merchant_id, completed_at);

-- Partial on exactly the expirable set: an already-expired or completed session
-- is not work, and without the predicate the sweep would re-read every historic
-- session of every merchant forever.
CREATE INDEX ix_checkout_sessions_expirable
    ON openrails.checkout_sessions USING btree (merchant_id, expires_at)
    WHERE (expires_at IS NOT NULL AND deleted_at IS NULL
           AND status IN ('created', 'requires_action'));

-- --------------------------------------------------------------------------
-- The durable sweep cursor
-- --------------------------------------------------------------------------

-- Operator-global control-plane table, like openrails.worker_health: it holds
-- no tenant data, only "which merchant did this worker stop at". No RLS.
CREATE TABLE openrails.worker_sweep_cursors (
    worker_kind text NOT NULL,
    -- NULL = start of the ring. A pass that drained its queue clears the cursor
    -- so the next pass re-reads from the beginning.
    cursor_merchant_id uuid,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT worker_sweep_cursors_pkey PRIMARY KEY (worker_kind)
);

COMMENT ON TABLE openrails.worker_sweep_cursors IS 'RLS-exempt by design: or#837 resume point for capped fan-out sweeps — the last merchant id a bounded pass handled. A cap without a cursor re-serves the same head every tick and starves the tail; a cursor without a cap is the unbounded enumeration this replaced. Operator-global process state, no tenant data (see worker_health).';

COMMENT ON COLUMN openrails.worker_sweep_cursors.cursor_merchant_id IS
    'Exclusive lower bound for the next pass. NULL = the previous pass drained its work queue, so the next one starts from the beginning.';

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.worker_sweep_cursors TO openrails_app;

-- --------------------------------------------------------------------------
-- The work queue
-- --------------------------------------------------------------------------

-- Merchants holding at least one row past one of the retention cutoffs. Each
-- leg carries the SAME predicate as the sweep statement it stands for, minus
-- the row payload, so a merchant whose only due row was deleted by a concurrent
-- pass costs one empty indexed scan rather than a wrong answer.
--
-- Every leg is independently capped and cursored, so one enormous table cannot
-- crowd the others out of the union.
CREATE FUNCTION openrails.retention_work_merchant_ids(
        p_now timestamptz,
        p_notification_cutoff timestamptz,
        p_notification_seen_cutoff timestamptz,
        p_webhook_cutoff timestamptz,
        p_settlement_cutoff timestamptz,
        p_lifecycle_cutoff timestamptz,
        p_after uuid,
        p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
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
               FROM openrails.notification_queue nq
              WHERE (p_after IS NULL OR nq.merchant_id > p_after)
                AND (nq.created_at < p_notification_cutoff
                     OR (nq.seen AND nq.created_at < p_notification_seen_cutoff))
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT we.merchant_id AS mid
               FROM openrails.webhook_events we
              WHERE (p_after IS NULL OR we.merchant_id > p_after)
                AND we.completed_at < p_webhook_cutoff
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT pse.merchant_id AS mid
               FROM openrails.payment_settlement_events pse
              WHERE (p_after IS NULL OR pse.merchant_id > p_after)
                AND pse.delivered_at IS NOT NULL
                AND pse.delivered_at < p_settlement_cutoff
              ORDER BY 1 LIMIT p_limit)
            UNION
            (SELECT DISTINCT hle.merchant_id AS mid
               FROM openrails.host_lifecycle_events hle
              WHERE (p_after IS NULL OR hle.merchant_id > p_after)
                AND hle.delivered_at IS NOT NULL
                AND hle.delivered_at < p_lifecycle_cutoff
              ORDER BY 1 LIMIT p_limit)
           ) q
     ORDER BY q.mid
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.retention_work_merchant_ids(timestamptz, timestamptz, timestamptz, timestamptz, timestamptz, timestamptz, uuid, int) IS
    'or#837: merchants with retention work — an expirable checkout session past its TTL, a notification/webhook-dedup row past its window, or an ACKED settlement/host-lifecycle event past its prune age. The fan-out list for CleanupExpiredDataWorker, replacing a full walk of every active merchant every hour. Ids only, after a cursor, capped; the deletes run per-merchant under RunInMerchantScope in bounded batches.';

REVOKE ALL ON FUNCTION openrails.retention_work_merchant_ids(timestamptz, timestamptz, timestamptz, timestamptz, timestamptz, timestamptz, uuid, int) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION openrails.retention_work_merchant_ids(timestamptz, timestamptz, timestamptz, timestamptz, timestamptz, timestamptz, uuid, int) TO openrails_app;

-- --------------------------------------------------------------------------
-- or#837: urgency ordering for the dunning fan-out (0023)
-- --------------------------------------------------------------------------

-- 0023 capped the list but left the truncation ARBITRARY: `SELECT DISTINCT …
-- LIMIT n` with no ORDER BY serves whichever merchants the plan happened to
-- reach first, so at cap a merchant could be skipped pass after pass while its
-- retries aged. Most-overdue merchant first — the same urgency axis the
-- per-merchant scan now uses.
CREATE OR REPLACE FUNCTION openrails.due_dunning_merchant_ids(p_rails text[], p_now timestamptz, p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT s.merchant_id
      FROM openrails.subscriptions s
     WHERE s.rail = ANY(p_rails)
       AND s.status = 'past_due'
       AND s.next_retry_at IS NOT NULL AND s.next_retry_at <= p_now
       AND s.deleted_at IS NULL
     GROUP BY s.merchant_id
     ORDER BY MIN(s.next_retry_at)
     LIMIT p_limit;
END;
$$;
