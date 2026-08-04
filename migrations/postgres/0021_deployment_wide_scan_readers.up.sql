-- or#860 / or#861: the deployment-wide scans that GenGlobal() could never serve.
--
-- `DB.GenGlobal()` promises a cross-merchant read and cannot deliver one. There
-- is ONE pool and ONE role; dropping the app.merchant_id GUC does not bypass a
-- policy, it FAILS it, so under `openrails_app` every base-pool read of a
-- policy-bearing table returns ZERO ROWS AND NO ERROR (0016's header states the
-- same fact for the directory reads).
--
-- Three shipped controls were built on that promise and are therefore inert in
-- production:
--   * the #732 destructive-rate ceiling counted 0 destructive intents and so
--     NEVER TRIPPED — a fail-OPEN safety control on irreversible provider
--     writes (or#860, P1);
--   * the alert evaluator and the #787 low-severity findings digest selected no
--     armed merchants, so neither has ever run (or#861).
--
-- Each function below follows 0016's contract exactly:
--   * it asserts its definer actually bypasses RLS, so a mis-owned schema
--     RAISES instead of silently answering "nothing to do" — the failure mode
--     that produced this whole class must never be silent again;
--   * it exposes the NARROWEST possible projection. The counts return a scalar.
--     The armed scans return merchant ids ONLY — the work itself still runs
--     per-merchant under MerchantTx, so no row of merchant data ever leaves its
--     policy. A deployment-wide *scan* is legitimate; a deployment-wide *read*
--     of merchant rows is not, and nothing here grants one.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- --------------------------------------------------------------------------
-- #732 anti-credential-compromise rate ceiling (or#860)
-- --------------------------------------------------------------------------
-- The durable rail_intents ledger IS the counter (#674): a destructive op posts
-- a row BEFORE it executes, so a rolling-hour COUNT over created_at is the
-- burst gauge. Scoped to origin IN ('user','admin') — the credential-theft
-- surface; origin='system' is #679's volume breaker's job.

CREATE FUNCTION openrails.count_destructive_intents_since(
    p_intent_types text[], p_since timestamptz)
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
     WHERE origin IN ('user', 'admin')
       AND intent_type = ANY (p_intent_types)
       AND created_at >= p_since;
    RETURN n;
END;
$$;

COMMENT ON FUNCTION openrails.count_destructive_intents_since(text[], timestamptz) IS
    'Deployment-wide count of destructive user/admin intents created since a cutoff — the #732 rate ceiling''s global wall. A scalar, never rows. Replaces a base-pool count that returned 0 under RLS and so never tripped (or#860).';

CREATE FUNCTION openrails.count_destructive_intents_by_actor_since(
    p_actor text, p_intent_types text[], p_since timestamptz)
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
     WHERE actor = p_actor
       AND origin IN ('user', 'admin')
       AND intent_type = ANY (p_intent_types)
       AND created_at >= p_since;
    RETURN n;
END;
$$;

COMMENT ON FUNCTION openrails.count_destructive_intents_by_actor_since(text, text[], timestamptz) IS
    'Per-actor, cross-merchant count of destructive intents in the rolling window — the #732 ceiling''s more specific leg. One compromised credential operating across merchants is exactly the shape this must see.';

-- rail_intents had no index able to serve a rolling-window scan across
-- merchants; every existing index is merchant-first. Partial on the destructive
-- origins so the index stays small (a deployment posts far more system intents
-- than user/admin ones).
CREATE INDEX idx_rail_intents_destructive_window
    ON openrails.rail_intents USING btree (created_at, intent_type)
    WHERE (origin IN ('user', 'admin'));

CREATE INDEX idx_rail_intents_destructive_actor_window
    ON openrails.rail_intents USING btree (actor, created_at, intent_type)
    WHERE (origin IN ('user', 'admin'));

-- --------------------------------------------------------------------------
-- Armed-merchant scans (or#861)
-- --------------------------------------------------------------------------

CREATE FUNCTION openrails.armed_alert_merchant_ids()
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT r.merchant_id FROM openrails.alert_rules r WHERE r.enabled;
END;
$$;

COMMENT ON FUNCTION openrails.armed_alert_merchant_ids() IS
    'Merchants with at least one enabled alert rule — the evaluator''s work set. Ids only: the rules themselves are still read per-merchant under MerchantTx. Replaces a base-pool scan that selected nothing, so the evaluator had never run (or#861).';

CREATE FUNCTION openrails.armed_findings_digest_merchant_ids()
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT f.merchant_id
      FROM openrails.reconciliation_findings f
     WHERE f.status = 'requires_review' AND f.severity = 'low' AND f.notified_at IS NULL;
END;
$$;

COMMENT ON FUNCTION openrails.armed_findings_digest_merchant_ids() IS
    '#787: merchants holding at least one undigested low-severity requires_review finding. Ids only; the digest content is read per-merchant. Replaces a base-pool scan that selected nothing (or#861).';

REVOKE ALL ON FUNCTION openrails.count_destructive_intents_since(text[], timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.count_destructive_intents_by_actor_since(text, text[], timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.armed_alert_merchant_ids() FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.armed_findings_digest_merchant_ids() FROM PUBLIC;

GRANT EXECUTE ON FUNCTION openrails.count_destructive_intents_since(text[], timestamptz) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.count_destructive_intents_by_actor_since(text, text[], timestamptz) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.armed_alert_merchant_ids() TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.armed_findings_digest_merchant_ids() TO openrails_app;
