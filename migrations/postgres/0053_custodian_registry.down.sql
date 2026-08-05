-- Reverse or#880 phase 3: custody identity goes back into the charging PSP's
-- settings blob. Declared custodians are NOT folded back into PSP settings —
-- one custodian may back several PSPs, so the fold is lossy in the general
-- case; the rows are dropped and the arrangement must be re-declared inline.
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP FUNCTION IF EXISTS openrails.custodian_owner_by_identity(text, text, text);

DROP INDEX IF EXISTS openrails.idx_psps_custodian;
ALTER TABLE openrails.psps DROP CONSTRAINT IF EXISTS psps_custodian_fk;
ALTER TABLE openrails.psps DROP COLUMN IF EXISTS custodian_id;

DROP TABLE IF EXISTS openrails.custodians;

CREATE UNIQUE INDEX uq_psps_custodian_identity
    ON openrails.psps ((evidence -> 'settings' ->> 'custodian'),
                       environment,
                       (evidence -> 'settings' ->> 'custodian_account_id'))
 WHERE (evidence -> 'settings' ->> 'custodian_account_id') IS NOT NULL;

CREATE FUNCTION openrails.psp_owner_by_custodian_identity(p_custodian text, p_environment text, p_account_id text)
    RETURNS TABLE (id uuid, merchant_id uuid, rail text, environment text, account_id text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT p.id, p.merchant_id, p.rail, p.environment, p.account_id
      FROM openrails.psps p
     WHERE (p.evidence -> 'settings' ->> 'custodian') = lower(p_custodian)
       AND p.environment = p_environment
       AND (p.evidence -> 'settings' ->> 'custodian_account_id') = p_account_id
     LIMIT 1;
END;
$$;

REVOKE ALL ON FUNCTION openrails.psp_owner_by_custodian_identity(text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION openrails.psp_owner_by_custodian_identity(text, text, text) TO openrails_app;
