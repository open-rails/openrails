DROP FUNCTION openrails.psp_rail_merchant_ids(text[], integer, uuid);

CREATE FUNCTION openrails.psp_rail_merchant_ids(p_rails text[], p_limit integer) RETURNS TABLE(merchant_id uuid)
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

COMMENT ON FUNCTION openrails.psp_rail_merchant_ids(p_rails text[], p_limit integer) IS 'Merchants armed on at least one of the named rails (live PSP, undeleted merchant) — the fan-out list for StripeWebhookReconcileWorker and the alert-only catalog pull. Ids only; the PSP rows are read per-merchant under RunInMerchantScope. Replaces a merchants JOIN psps on the base pool, where the psps side is RLS-FORCED and always matched nothing — so the managed Stripe endpoint was never registered or version-bumped (or#877 B6).';

REVOKE ALL ON FUNCTION openrails.psp_rail_merchant_ids(p_rails text[], p_limit integer) FROM PUBLIC;
GRANT ALL ON FUNCTION openrails.psp_rail_merchant_ids(p_rails text[], p_limit integer) TO openrails_app;
