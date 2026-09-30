-- parent: 33 sha256:0578a2169d0c0f933bf3e3a5e855aa50da341b81eac8454183c43f23708590b8
-- #1111: a declined charge is a payment attempt, never a payments row, so the
-- fleet's per-rail approvals and declines count charge attempts (every kind
-- but the $0 verification). Chargebacks stay payments reversal rows.
CREATE OR REPLACE FUNCTION openrails.fleet_rail_health(p_exclude uuid, p_since timestamp with time zone) RETURNS TABLE(rail text, succeeded bigint, failed bigint, chargebacks bigint)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    WITH charges AS (
        SELECT a.rail AS r,
               count(*) FILTER (WHERE a.category = 'approved') AS ok,
               count(*) FILTER (WHERE a.category <> 'approved') AS refused
          FROM openrails.payment_attempts a
         WHERE a.attempted_at >= p_since AND a.kind <> 'verify'
           AND (p_exclude IS NULL OR a.merchant_id <> p_exclude)
         GROUP BY a.rail
    ), disputes AS (
        SELECT p.rail::text AS r, count(*) AS n
          FROM openrails.payments p
         WHERE p.purchased_at >= p_since AND p.reversal_kind = 'chargeback' AND p.status = 'completed'
           AND (p_exclude IS NULL OR p.merchant_id <> p_exclude)
         GROUP BY p.rail
    )
    SELECT COALESCE(c.r, d.r), COALESCE(c.ok, 0)::bigint, COALESCE(c.refused, 0)::bigint, COALESCE(d.n, 0)::bigint
      FROM charges c FULL JOIN disputes d ON d.r = c.r
     ORDER BY 1;
END;
$$;
COMMENT ON FUNCTION openrails.fleet_rail_health(p_exclude uuid, p_since timestamp with time zone) IS 'Per-rail fleet approved/declined charge attempts and chargebacks in the window (or#861, #1111).';
