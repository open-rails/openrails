-- or#862 / or#868 / or#861: the rest of the deployment-wide work that
-- `GenGlobal()` and a bare `RunInTx` could never serve.
--
-- The standing fact, restated once more because it is the root of this whole
-- class: there is ONE pool and ONE role. Dropping the app.merchant_id GUC does
-- not bypass a policy, it FAILS it — a base-pool read of a policy-bearing table
-- under `openrails_app` returns ZERO ROWS AND NO ERROR, and a base-pool write
-- is denied outright with 42501. 0016 established the sanctioned escape hatch;
-- 0021 used it for the destructive-rate ceiling and the armed scans. This
-- migration finishes the sweep:
--
--   * or#862 (P1) — the provider-intent executor/verifier ran `ClaimDue` on a
--     bare River job context, so it claimed ZERO intents. The whole outbound
--     provider-mutation plane was inert, and with it the #836 kill switch and
--     the #679 volume breaker, which only ever run on a claimed intent.
--   * or#868 B1 — the credit-lot expiry worker enumerated lapsed lots on the
--     base pool and has therefore NEVER expired a lot.
--   * or#861 — the #816 plan-migration re-driver never re-drove, and the hosted
--     fleet dashboard reported all zeros.
--
-- Same contract as 0016/0021, without exception:
--   * every function ASSERTS its definer actually bypasses RLS, so a mis-owned
--     schema RAISES instead of silently answering "no work to do";
--   * the work queues return merchant IDS ONLY. The work itself still runs
--     per-merchant inside RunInMerchantConn, under the merchant's own policy —
--     a deployment-wide SCAN is legitimate, a deployment-wide READ of merchant
--     rows is not, and nothing here grants one;
--   * the fleet dashboard returns AGGREGATES ONLY (counts and sums, grouped by
--     currency/rail/week). It is the one genuinely cross-merchant READ in the
--     product, it exists for the platform operator, and it still never vends a
--     merchant-owned row.

SET statement_timeout = '60s';
SET lock_timeout = '10s';

-- --------------------------------------------------------------------------
-- or#862: provider-intent executor / verifier work queues
-- --------------------------------------------------------------------------

-- Merchants with executor work: a claimable intent (due pending/retryable, or
-- an orphaned in_flight lease) or an intent past its relevance window for the
-- expiry sweep. One pass per merchant then runs ClaimDue/ExpireOverdue under
-- that merchant's GUC, where the same predicates re-apply.
CREATE FUNCTION openrails.due_rail_intent_merchant_ids(p_now timestamptz, p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT i.merchant_id
      FROM openrails.rail_intents i
     WHERE (
             -- claimable now
             (
               (
                 (i.status IN ('pending', 'failed_retryable') AND i.next_attempt_at <= p_now)
                 OR (i.status = 'in_flight' AND i.claimed_until IS NOT NULL AND i.claimed_until <= p_now)
               )
               AND (i.expires_at IS NULL OR i.expires_at > p_now)
             )
             -- or expirable by the same pass's ExpireOverdue leg
             OR (
               i.status IN ('pending', 'failed_retryable', 'unknown_needs_verify')
               AND i.expires_at IS NOT NULL AND i.expires_at <= p_now
             )
           )
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.due_rail_intent_merchant_ids(timestamptz, int) IS
    'Merchants with provider-intent executor work due — the fan-out list for ProviderIntentExecuteWorker. Ids only; the claim, the gates and the execution all run per-merchant under RunInMerchantConn. Replaces a bare-context ClaimDue that claimed zero intents, disarming the #836 kill switch and the #679 volume breaker with it (or#862).';

CREATE FUNCTION openrails.due_verify_rail_intent_merchant_ids(p_now timestamptz, p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT i.merchant_id
      FROM openrails.rail_intents i
     WHERE i.status = 'unknown_needs_verify'
       AND i.next_attempt_at <= p_now
       AND (i.claimed_until IS NULL OR i.claimed_until <= p_now)
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.due_verify_rail_intent_merchant_ids(timestamptz, int) IS
    'Merchants with ambiguous intents due for provider-read verification — the fan-out list for ProviderIntentVerifyWorker. Ids only (or#862).';

-- --------------------------------------------------------------------------
-- or#868 B1: credit-lot expiry
-- --------------------------------------------------------------------------

-- Deliberately cheaper than ListCustomersWithLapsedCreditLots: it only needs to
-- know WHICH merchants have a lapsed lot, so it stops at the expiry predicate
-- and leaves the unspent-remainder arithmetic to the per-merchant pass (which
-- runs the full query under the merchant's own GUC). A merchant whose lots are
-- all already clawed back costs one empty per-merchant pass, not a wrong answer.
CREATE FUNCTION openrails.lapsed_credit_lot_merchant_ids(p_as_of timestamptz, p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT g.merchant_id
      FROM openrails.grants g
     WHERE g.kind = 'credit' AND g.event = 'grant'
       AND g.ends_at IS NOT NULL AND g.ends_at <= p_as_of
       AND NOT EXISTS (
             SELECT 1 FROM openrails.grants tt
              WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'supersede')
           )
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.lapsed_credit_lot_merchant_ids(timestamptz, int) IS
    'Merchants holding at least one past-expiry, non-superseded credit lot — the fan-out list for CreditExpiryWorker. Ids only; the per-customer work list and the ledger claw-back run per-merchant. Replaces a base-pool enumeration that returned nothing, so no credit lot has ever been expired (or#868 B1).';

-- --------------------------------------------------------------------------
-- or#861: #816 plan-migration re-driver
-- --------------------------------------------------------------------------

CREATE FUNCTION openrails.redrivable_plan_change_merchant_ids(p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT r.merchant_id
      FROM openrails.subscription_reprices r
     WHERE r.kind = 'plan_change'
       AND r.status = 'blocked'
       AND r.blocked_reason LIKE 'rail_push_failed:%'
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.redrivable_plan_change_merchant_ids(int) IS
    '#816: merchants holding a rail-push-blocked plan_change reprice. Ids only — unlike the armed scans the re-driver needs whole rows, and a definer must not vend those, so the rows are read per-merchant under RunInMerchantConn (or#861).';

-- subscription_reprices had no index able to find blocked rows across
-- merchants (idx_subscription_reprices_due is scheduled-only, the merchant one
-- is merchant-first). Partial on the blocked status so it stays small.
CREATE INDEX idx_subscription_reprices_blocked_plan_change
    ON openrails.subscription_reprices USING btree (merchant_id)
    WHERE (status = 'blocked' AND kind = 'plan_change');

-- --------------------------------------------------------------------------
-- or#861: hosted fleet dashboard (openrails-saas #28 / #38)
-- --------------------------------------------------------------------------
-- AGGREGATES ONLY. p_exclude drops one merchant (the platform's own
-- self-billing book) from every aggregate; NULL excludes nothing.

CREATE FUNCTION openrails.fleet_merchant_funnel(p_exclude uuid, p_since timestamptz)
    RETURNS TABLE (total bigint, armed bigint, first_revenue bigint, active_revenue bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT count(*)::bigint,
           (count(*) FILTER (WHERE EXISTS (
               SELECT 1 FROM openrails.psps p
                WHERE p.merchant_id = m.id AND p.replaced_at IS NULL)))::bigint,
           (count(*) FILTER (WHERE EXISTS (
               SELECT 1 FROM openrails.payments pay
                WHERE pay.merchant_id = m.id AND pay.status = 'completed'
                  AND pay.reversal_kind IS NULL)))::bigint,
           (count(*) FILTER (WHERE EXISTS (
               SELECT 1 FROM openrails.payments pay
                WHERE pay.merchant_id = m.id AND pay.status = 'completed'
                  AND pay.reversal_kind IS NULL
                  AND pay.purchased_at >= p_since)))::bigint
      FROM openrails.merchants m
     WHERE m.deleted_at IS NULL AND m.status = 'active'
       AND (p_exclude IS NULL OR m.id <> p_exclude);
END;
$$;

COMMENT ON FUNCTION openrails.fleet_merchant_funnel(uuid, timestamptz) IS
    'Fleet funnel counters (provisioned / armed / ever-earned / earning-now). Four scalars, no rows (or#861).';

CREATE FUNCTION openrails.fleet_revenue_by_currency(p_exclude uuid, p_since timestamptz)
    RETURNS TABLE (currency text, payments bigint, settled_amount bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT p.currency::text, count(*)::bigint, COALESCE(sum(p.amount), 0)::bigint
      FROM openrails.payments p
     WHERE p.status = 'completed' AND p.reversal_kind IS NULL AND p.purchased_at >= p_since
       AND (p_exclude IS NULL OR p.merchant_id <> p_exclude)
     GROUP BY p.currency ORDER BY p.currency;
END;
$$;

COMMENT ON FUNCTION openrails.fleet_revenue_by_currency(uuid, timestamptz) IS
    'Settled fleet sale volume per currency in the window. Sale rows only — reversal mirror rows share status=completed and must never count (or#861).';

CREATE FUNCTION openrails.fleet_rail_health(p_exclude uuid, p_since timestamptz)
    RETURNS TABLE (rail text, succeeded bigint, failed bigint, chargebacks bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT p.rail::text,
           (count(*) FILTER (WHERE p.status = 'completed' AND p.reversal_kind IS NULL))::bigint,
           (count(*) FILTER (WHERE p.status = 'failed' AND p.reversal_kind IS NULL))::bigint,
           (count(*) FILTER (WHERE p.reversal_kind = 'chargeback'))::bigint
      FROM openrails.payments p
     WHERE p.purchased_at >= p_since AND p.status IN ('completed', 'failed')
       AND (p_exclude IS NULL OR p.merchant_id <> p_exclude)
     GROUP BY p.rail ORDER BY p.rail;
END;
$$;

COMMENT ON FUNCTION openrails.fleet_rail_health(uuid, timestamptz) IS
    'Per-rail fleet approval/decline/chargeback counters in the window (or#861).';

CREATE FUNCTION openrails.fleet_mrr_by_currency(p_exclude uuid)
    RETURNS TABLE (currency text, subscriptions bigint, monthly_amount bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT pr.currency::text, count(*)::bigint,
           COALESCE(sum((pr.amount::numeric * 720 / pr.access_duration_hours)::bigint), 0)::bigint
      FROM openrails.subscriptions s
      JOIN openrails.prices pr ON pr.id = s.price_id
     WHERE s.status = 'active' AND pr.auto_renew AND pr.access_duration_hours > 0
       AND (p_exclude IS NULL OR s.merchant_id <> p_exclude)
     GROUP BY pr.currency ORDER BY pr.currency;
END;
$$;

COMMENT ON FUNCTION openrails.fleet_mrr_by_currency(uuid) IS
    'Fleet MRR per currency, normalised to a 720-hour month from each price''s access window (or#861).';

CREATE FUNCTION openrails.fleet_weekly_active_merchants(p_exclude uuid, p_since timestamptz)
    RETURNS TABLE (week_start timestamptz, merchants bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT date_trunc('week', p.purchased_at), (count(DISTINCT p.merchant_id))::bigint
      FROM openrails.payments p
     WHERE p.status = 'completed' AND p.reversal_kind IS NULL
       AND p.purchased_at >= date_trunc('week', p_since)
       AND (p_exclude IS NULL OR p.merchant_id <> p_exclude)
     GROUP BY 1;
END;
$$;

COMMENT ON FUNCTION openrails.fleet_weekly_active_merchants(uuid, timestamptz) IS
    'Weekly count of DISTINCT merchants with a settled sale — a count per ISO week, never the merchant list (or#861).';

CREATE FUNCTION openrails.fleet_weekly_cancelled_subscriptions(p_exclude uuid, p_since timestamptz)
    RETURNS TABLE (week_start timestamptz, cancellations bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT date_trunc('week', s.cancelled_at), count(*)::bigint
      FROM openrails.subscriptions s
     WHERE s.cancelled_at IS NOT NULL
       AND s.cancelled_at >= date_trunc('week', p_since)
       AND (p_exclude IS NULL OR s.merchant_id <> p_exclude)
     GROUP BY 1;
END;
$$;

COMMENT ON FUNCTION openrails.fleet_weekly_cancelled_subscriptions(uuid, timestamptz) IS
    'Weekly fleet subscription cancellations — the churn proxy on the fleet trend chart (or#861).';

CREATE FUNCTION openrails.fleet_weekly_volume(p_exclude uuid, p_since timestamptz)
    RETURNS TABLE (week_start timestamptz, currency text, payments bigint, settled_amount bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT date_trunc('week', p.purchased_at), p.currency::text, count(*)::bigint, COALESCE(sum(p.amount), 0)::bigint
      FROM openrails.payments p
     WHERE p.status = 'completed' AND p.reversal_kind IS NULL
       AND p.purchased_at >= date_trunc('week', p_since)
       AND (p_exclude IS NULL OR p.merchant_id <> p_exclude)
     GROUP BY 1, 2 ORDER BY 1, 2;
END;
$$;

COMMENT ON FUNCTION openrails.fleet_weekly_volume(uuid, timestamptz) IS
    'Weekly settled fleet sale volume per currency. Sale rows only (or#861).';

REVOKE ALL ON FUNCTION openrails.due_rail_intent_merchant_ids(timestamptz, int) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.due_verify_rail_intent_merchant_ids(timestamptz, int) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.lapsed_credit_lot_merchant_ids(timestamptz, int) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.redrivable_plan_change_merchant_ids(int) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.fleet_merchant_funnel(uuid, timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.fleet_revenue_by_currency(uuid, timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.fleet_rail_health(uuid, timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.fleet_mrr_by_currency(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.fleet_weekly_active_merchants(uuid, timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.fleet_weekly_cancelled_subscriptions(uuid, timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.fleet_weekly_volume(uuid, timestamptz) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION openrails.due_rail_intent_merchant_ids(timestamptz, int) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.due_verify_rail_intent_merchant_ids(timestamptz, int) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.lapsed_credit_lot_merchant_ids(timestamptz, int) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.redrivable_plan_change_merchant_ids(int) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.fleet_merchant_funnel(uuid, timestamptz) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.fleet_revenue_by_currency(uuid, timestamptz) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.fleet_rail_health(uuid, timestamptz) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.fleet_mrr_by_currency(uuid) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.fleet_weekly_active_merchants(uuid, timestamptz) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.fleet_weekly_cancelled_subscriptions(uuid, timestamptz) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.fleet_weekly_volume(uuid, timestamptz) TO openrails_app;
