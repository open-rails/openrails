-- parent: 29 sha256:08bb4d987ad4c3a93ac7dff8c7cbfe5fd306fe8a7b6951a8ad85c278f18cff42
-- #1112: a rebill that never happened is recorded as missed on its cycle.
-- The watch finds auto-renewing subscriptions whose paid period ended past the
-- owner's deadline with no attempt for that cycle.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.rebill_cycles
    ADD COLUMN missed_at timestamp with time zone,
    ADD COLUMN miss_reason text;
ALTER TABLE openrails.rebill_cycles
    ADD CONSTRAINT chk_rebill_cycles_missed CHECK ((missed_at IS NULL) = (miss_reason IS NULL)) NOT VALID;
ALTER TABLE openrails.rebill_cycles
    ADD CONSTRAINT chk_rebill_cycles_miss_reason CHECK (miss_reason IN ('held', 'refused', 'method_unusable', 'not_attempted', 'provider_skipped', 'provider_stalled', 'schedule_gone')) NOT VALID;

COMMENT ON COLUMN openrails.rebill_cycles.missed_at IS '#1112 when the cycle passed its owner''s deadline with no attempt; a later attempt still attaches to the cycle.';

CREATE INDEX idx_subscriptions_rebill_watch ON openrails.subscriptions USING btree (merchant_id, current_period_ends_at)
    WHERE status IN ('active', 'unverified', 'awaiting_method') AND collection_policy IN ('engine', 'nmi_schedule') AND deleted_at IS NULL;

-- The merchants holding a subscription whose period ended before its owner's
-- deadline with neither an attempt nor a recorded miss for that cycle.
CREATE FUNCTION openrails.overdue_rebill_merchant_ids(p_engine_cutoff timestamp with time zone, p_nmi_cutoff timestamp with time zone, p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT s.merchant_id
      FROM openrails.subscriptions s
     WHERE s.status IN ('active', 'unverified', 'awaiting_method') AND s.deleted_at IS NULL
       AND ((s.collection_policy = 'engine' AND s.current_period_ends_at <= p_engine_cutoff)
            OR (s.collection_policy = 'nmi_schedule' AND s.current_period_ends_at <= p_nmi_cutoff))
       AND NOT EXISTS (
           SELECT 1 FROM openrails.rebill_cycles c
            WHERE c.merchant_id = s.merchant_id AND c.subscription_id = s.id AND c.due_at = s.current_period_ends_at
              AND (c.missed_at IS NOT NULL OR EXISTS (SELECT 1 FROM openrails.payment_attempts a WHERE a.merchant_id = c.merchant_id AND a.cycle_id = c.id)))
     GROUP BY s.merchant_id
     ORDER BY MIN(s.current_period_ends_at), s.merchant_id
     LIMIT p_limit;
END;
$$;

REVOKE ALL ON FUNCTION openrails.overdue_rebill_merchant_ids(p_engine_cutoff timestamp with time zone, p_nmi_cutoff timestamp with time zone, p_limit integer) FROM PUBLIC;
