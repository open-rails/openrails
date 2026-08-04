-- or#877: the last four scheduled workers that read a policied table on the
-- bare River job context, plus the two FC-16 found.
--
-- Same standing fact as 0016/0021/0022: there is ONE pool and ONE role.
-- Dropping the app.merchant_id GUC does not bypass a policy, it FAILS it — a
-- base-pool read under `openrails_app` matches `merchant_id = NULL` and returns
-- ZERO ROWS AND NO ERROR. That is why scheduled dunning has never found a due
-- subscription, why the managed Stripe webhook endpoint has never been
-- registered by its own reconciler, and why the alert-only catalog pull has
-- always diffed an EMPTY local catalog against the provider.
--
-- Same contract, without exception:
--   * every function ASSERTS its definer bypasses RLS, so a mis-owned schema
--     RAISES instead of silently answering "no work to do";
--   * the work queues return merchant IDS ONLY. The rows themselves are still
--     read per-merchant inside RunInMerchantScope, under the merchant's own
--     policy.

SET statement_timeout = '60s';
SET lock_timeout = '10s';

-- --------------------------------------------------------------------------
-- or#877 B5: dunning
-- --------------------------------------------------------------------------

-- Merchants holding a past_due subscription whose retry is due on one of the
-- named rails. Deliberately the SAME predicate as ListDueDunningSubscriptions
-- minus the row payload, so the per-merchant pass re-applies it under the
-- merchant's GUC and a merchant whose only due row was just claimed by a
-- concurrent pass costs one empty scan, not a wrong answer.
CREATE FUNCTION openrails.due_dunning_merchant_ids(p_rails text[], p_now timestamptz, p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT s.merchant_id
      FROM openrails.subscriptions s
     WHERE s.rail = ANY(p_rails)
       AND s.status = 'past_due'
       AND s.next_retry_at IS NOT NULL AND s.next_retry_at <= p_now
       AND s.deleted_at IS NULL
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.due_dunning_merchant_ids(text[], timestamptz, int) IS
    'Merchants with a due past_due subscription on the named rails — the fan-out list for DunningWorker. Ids only; the due rows, the charges and every lifecycle transition run per-merchant under RunInMerchantScope. Replaces a bare-context scan that returned an empty slice on every run, so scheduled dunning (retries, #839 staleness parking, #840 terminal handling) never fired at all (or#877 B5).';

-- --------------------------------------------------------------------------
-- or#877 B6 + the FC-16 catalog pull: merchants armed on a rail
-- --------------------------------------------------------------------------

-- Merchants with a live (non-archived) PSP on one of the named rails. Archived
-- accounts keep their secrets for draining but take no new work, so they are
-- excluded here exactly as the callers' own predicates exclude them.
CREATE FUNCTION openrails.psp_rail_merchant_ids(p_rails text[], p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT p.merchant_id
      FROM openrails.psps p
      JOIN openrails.merchants m ON m.id = p.merchant_id
     WHERE p.rail = ANY(p_rails)
       AND p.archived = false
       AND m.deleted_at IS NULL
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.psp_rail_merchant_ids(text[], int) IS
    'Merchants armed on at least one of the named rails (live PSP, undeleted merchant) — the fan-out list for StripeWebhookReconcileWorker and the alert-only catalog pull. Ids only; the PSP rows are read per-merchant under RunInMerchantScope. Replaces a merchants JOIN psps on the base pool, where the psps side is RLS-FORCED and always matched nothing — so the managed Stripe endpoint was never registered or version-bumped (or#877 B6).';

REVOKE ALL ON FUNCTION openrails.due_dunning_merchant_ids(text[], timestamptz, int) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.psp_rail_merchant_ids(text[], int) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION openrails.due_dunning_merchant_ids(text[], timestamptz, int) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.psp_rail_merchant_ids(text[], int) TO openrails_app;
