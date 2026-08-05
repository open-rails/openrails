-- Restore 0021's deployment-wide destructive-intent reader and its index.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

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

REVOKE ALL ON FUNCTION openrails.count_destructive_intents_since(text[], timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION openrails.count_destructive_intents_since(text[], timestamptz) TO openrails_app;

CREATE INDEX idx_rail_intents_destructive_window
    ON openrails.rail_intents USING btree (created_at, intent_type)
    WHERE (origin IN ('user', 'admin'));
