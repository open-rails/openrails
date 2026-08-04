-- or#842: the #732 destructive-rate ceiling was INERT for origin='system'.
--
-- 0021 armed the ceiling's counters but scoped both readers to
-- origin IN ('user','admin') — the credential-theft surface — leaving the
-- automated paths (dunning exhaustion, unknown-resolution convergence, the
-- decider's terminal cancels) with no producer-side wall at all. Those are the
-- paths that queue the most irreversible work, and they queue it without any
-- human in the loop.
--
-- The system ceiling is deliberately PER MERCHANT, not deployment-wide. A flat
-- global wall on system origin does not survive fleet scale: a thousand
-- merchants each converging their own book legitimately exceed any single
-- number, and stopping all of them because the fleet is large is a worse
-- failure than the one being prevented. A per-merchant window scales linearly
-- with the fleet while still walling off a runaway loop or a poisoned roster
-- inside ONE merchant — and the deployment-wide control for system origin
-- remains #679's per-merchant volume breaker at the executor.
--
-- Same contract as 0021: SECURITY DEFINER, asserts its owner actually bypasses
-- RLS (a mis-owned schema RAISES instead of silently answering 0 — the
-- fail-OPEN failure mode that made this whole class invisible), and returns a
-- scalar, never rows.

SET statement_timeout = '60s';
SET lock_timeout = '10s';

CREATE FUNCTION openrails.count_system_destructive_intents_for_merchant_since(
    p_merchant uuid, p_intent_types text[], p_since timestamptz)
    RETURNS bigint
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
DECLARE
    n bigint;
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    SELECT count(*) INTO n
      FROM openrails.rail_intents
     WHERE merchant_id = p_merchant
       AND origin = 'system'
       AND intent_type = ANY (p_intent_types)
       AND created_at >= p_since;
    RETURN n;
END;
$$;

COMMENT ON FUNCTION openrails.count_system_destructive_intents_for_merchant_since(uuid, text[], timestamptz) IS
    'One merchant''s AUTOMATED (origin=system) destructive intents in a rolling window — the #732 ceiling''s system-origin leg (or#842). Definer, not a base-pool read: the ceiling holds the root pool and carries no app.merchant_id, where a GUC-less count would return 0 and fail open.';

-- 0021's destructive-window indexes are partial on origin IN ('user','admin')
-- and merchant-agnostic; neither serves this predicate.
CREATE INDEX idx_rail_intents_system_destructive_window
    ON openrails.rail_intents USING btree (merchant_id, created_at, intent_type)
    WHERE (origin = 'system');

REVOKE ALL ON FUNCTION openrails.count_system_destructive_intents_for_merchant_since(uuid, text[], timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION openrails.count_system_destructive_intents_for_merchant_since(uuid, text[], timestamptz) TO openrails_app;
