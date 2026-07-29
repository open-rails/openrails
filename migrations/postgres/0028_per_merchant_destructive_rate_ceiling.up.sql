-- or#887: the #732 anti-credential-compromise ceiling was DEPLOYMENT-WIDE, which
-- makes it a cross-tenant denial of service.
--
-- 15 destructive ops per rolling hour is the right wall for a stolen credential;
-- sharing that one budget across every merchant is not. Merchant A's ordinary
-- customer cancellations exhaust it and merchant B's next cancellation is
-- refused — on a platform designed for thousands of merchants, the first busy
-- tenant permanently denies service to everyone else. The anti-theft property
-- does not need a shared budget: a forged-identity burst is still walled at 15
-- inside whatever merchant it targets, and the blast radius is now the tenant it
-- came from. (Same reasoning or#842 already applied to the system-origin leg.)
--
-- So the ceiling's merchant-scoped reader becomes the ONE reader for both legs:
-- 0024's origin='system' function is GENERALIZED to take the origin set as a
-- parameter, and the anti-theft leg (origin IN ('user','admin')) passes its own.
-- Two near-identical definers would have been the alternative; the origin list
-- is the only thing that differed.
--
-- The SECURITY DEFINER contract is UNCHANGED and deliberate. A per-merchant
-- count could in principle run under ordinary RLS now — but the gate holds the
-- ROOT pool (its counts and its findings must not ride the caller's transaction,
-- which the refusal rolls back), and that pool carries no app.merchant_id: under
-- FORCEd RLS a plain SELECT would match `merchant_id = NULL`, count 0, and fail
-- OPEN — exactly or#824/or#860. Switching mechanism would mean re-plumbing the
-- gate onto a merchant-pinned connection and giving up that independence. So:
-- definer, and it still asserts its owner can bypass RLS, so a mis-owned schema
-- RAISES instead of silently permitting unlimited destructive intents.

SET statement_timeout = '60s';
SET lock_timeout = '10s';

CREATE FUNCTION openrails.count_destructive_intents_for_merchant_since(
    p_merchant uuid, p_origins text[], p_intent_types text[], p_since timestamptz)
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
       AND origin = ANY (p_origins)
       AND intent_type = ANY (p_intent_types)
       AND created_at >= p_since;
    RETURN n;
END;
$$;

COMMENT ON FUNCTION openrails.count_destructive_intents_for_merchant_since(uuid, text[], text[], timestamptz) IS
    'ONE merchant''s destructive intents in a rolling window, for a caller-supplied origin set — both legs of the #732 ceiling: the anti-theft wall (user/admin, or#887) and the automation wall (system, or#842). Definer, not a base-pool read: the gate holds the root pool and carries no app.merchant_id, where a GUC-less count would return 0 and fail open.';

REVOKE ALL ON FUNCTION openrails.count_destructive_intents_for_merchant_since(uuid, text[], text[], timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION openrails.count_destructive_intents_for_merchant_since(uuid, text[], text[], timestamptz) TO openrails_app;

DROP FUNCTION openrails.count_system_destructive_intents_for_merchant_since(uuid, text[], timestamptz);

-- The origin set is now a runtime array, so neither 0021's partial index
-- (origin IN ('user','admin'), merchant-agnostic) nor 0024's (origin='system')
-- can be proven to satisfy the predicate. One full index with origin as a
-- leading equality column serves both legs.
CREATE INDEX idx_rail_intents_merchant_destructive_window
    ON openrails.rail_intents USING btree (merchant_id, origin, created_at, intent_type);
DROP INDEX IF EXISTS openrails.idx_rail_intents_system_destructive_window;

-- Persisted findings: the tripped/warning findings the deployment-wide wall
-- raised are keyed on the literal subject 'global', which no longer names
-- anything the gate can raise or re-raise. They are re-keyed, NOT deleted: each
-- one records a real burst an operator has not yet acknowledged, and the row is
-- already merchant-scoped, so 'merchant:<id>' is the same event under the name
-- the gate now uses. Their stored evidence keeps the old global_* keys — it is
-- the historical record of what was observed, not a live reading.
UPDATE openrails.reconciliation_findings
   SET subject_key = 'merchant:' || merchant_id::text
 WHERE finding_type IN ('life.destructive_rate.tripped', 'life.destructive_rate.warning')
   AND subject_key = 'global';

COMMENT ON COLUMN openrails.rail_intents.actor IS
    'Authenticated principal id (admin user id or self-service customer id) that produced a user/admin-origin intent. NULL for system-origin. Powers the #732 anti-credential-compromise rate ceiling (per-actor + per-merchant rolling-hour count of destructive ops).';
