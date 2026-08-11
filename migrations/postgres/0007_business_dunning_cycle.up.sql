-- or#910: dunning + budget-alert cycle.
--
-- 1. Suspension-RECOMMENDATION state on the business profile. OpenRails never
--    suspends anyone — it recommends, durably and edge-triggered, and the host
--    enforces (the or#878 delinquency doctrine, applied to the B2B invoice
--    ladder). The pair of columns is the episode watermark: recommended_at is
--    set exactly once per episode (UPDATE ... WHERE suspension_recommended_at
--    IS NULL) and cleared when every past-due receivable is settled, so racing
--    evaluators collapse onto one signal in each direction.
--
-- 2. The cycle's cross-merchant work queue, mirroring
--    delinquency_work_merchant_ids: merchants with at least one onboarded
--    business customer. Ids only; everything else is computed per-merchant
--    under RunInMerchantScope.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.customer_business_profiles
    ADD COLUMN suspension_recommended_at timestamp with time zone,
    ADD COLUMN suspension_reason text DEFAULT ''::text NOT NULL;

COMMENT ON COLUMN openrails.customer_business_profiles.suspension_recommended_at IS 'or#910: when the dunning cycle last RECOMMENDED suspension (a signal — hosts enforce; OpenRails never revokes access). NULL = no open recommendation. Set once per episode, cleared when the past-due book is settled.';
COMMENT ON COLUMN openrails.customer_business_profiles.suspension_reason IS 'or#910: the operator-readable reason behind the open recommendation ("invoice INV-7 unpaid 15 days past due"). Empty when no recommendation is open.';

CREATE FUNCTION openrails.business_cycle_work_merchant_ids(p_limit integer) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT p.merchant_id
      FROM openrails.customer_business_profiles p
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.business_cycle_work_merchant_ids(p_limit integer) IS 'or#910: merchants with at least one onboarded business customer — the fan-out list for BusinessCycleWorker (dunning ladder + budget alerts). Ids only; notices, recommendation edges and alerts are computed per-merchant under RunInMerchantScope.';

REVOKE ALL ON FUNCTION openrails.business_cycle_work_merchant_ids(p_limit integer) FROM PUBLIC;
GRANT ALL ON FUNCTION openrails.business_cycle_work_merchant_ids(p_limit integer) TO openrails_app;
