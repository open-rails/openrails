-- Reverting restores the DEPLOYMENT-WIDE anti-theft budget, i.e. one merchant's
-- ordinary destructive traffic can again refuse every other merchant's. Only do
-- this alongside reverting the caller (intents.RateCeiling).
--
-- Findings re-keyed to 'merchant:<id>' are NOT keyed back: the gate that owns
-- 'global' is gone at this point either way, and rewriting an operator's open
-- alerts twice is worse than leaving them where they are readable.

DROP INDEX IF EXISTS openrails.idx_rail_intents_merchant_destructive_window;

CREATE INDEX idx_rail_intents_system_destructive_window
    ON openrails.rail_intents USING btree (merchant_id, created_at, intent_type)
    WHERE (origin = 'system');

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

REVOKE ALL ON FUNCTION openrails.count_system_destructive_intents_for_merchant_since(uuid, text[], timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION openrails.count_system_destructive_intents_for_merchant_since(uuid, text[], timestamptz) TO openrails_app;

DROP FUNCTION IF EXISTS openrails.count_destructive_intents_for_merchant_since(uuid, text[], text[], timestamptz);
