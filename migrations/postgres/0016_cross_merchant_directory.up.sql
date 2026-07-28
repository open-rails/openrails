-- #824 / SEC-18: an EXPLICIT cross-merchant read path, and a reusable
-- merchant-scope predicate for by-id admin queries.
--
-- Background. There is no "privileged pool". internal/db builds ONE pgxpool and
-- every caller shares it; "privileged vs RLS" is only whether a statement
-- carries the app.merchant_id GUC. Pool.Query/QueryRow set no GUC, so under the
-- unprivileged openrails_app role a read of any policy-bearing table matches
-- `merchant_id = NULL` and returns ZERO ROWS AND NO ERROR. Three call sites
-- were commented "runs on the privileged, non-RLS role" and relied on
-- cross-merchant visibility they never had — the worst of them made every
-- account-routed CCBill/Basis Theory/Stripe-account webhook 404.
--
-- Cross-merchant reads that genuinely have no merchant yet (webhook routing,
-- global PSP ownership, a hosted portal's "which merchants is this subject a
-- customer of") now go through these SECURITY DEFINER functions instead of an
-- assumption about the pool. Each one:
--   * exposes a single, narrow projection — never the whole row, never a
--     listing: you must already name the exact global identity you are
--     resolving, and you learn only who owns it;
--   * ASSERTS that its definer actually bypasses RLS. If the migration owner is
--     neither superuser nor BYPASSRLS the function RAISES instead of returning
--     an empty set, so the failure that produced #824 can never be silent again.

-- Lock/statement safety (or#838 migration gate), same shape as the 0001 baseline.
SET statement_timeout = '60s';
SET lock_timeout = '10s';

CREATE FUNCTION openrails.current_merchant_id() RETURNS uuid
    LANGUAGE sql STABLE
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
    SELECT NULLIF(current_setting('app.merchant_id', true), '')::uuid
$$;

COMMENT ON FUNCTION openrails.current_merchant_id() IS
    'The request''s merchant from the app.merchant_id GUC, or NULL when unset. Same expression the merchant_isolation RLS policies use, so a query carrying `merchant_id = openrails.current_merchant_id()` enforces the SAME scope in the application layer — defence in depth for by-id admin surfaces whose only other control is a role that might bypass RLS (SEC-18).';

-- Guard shared by every definer function below. SECURITY INVOKER on purpose:
-- called from inside a definer body, current_user is that function's OWNER,
-- which is exactly the role whose privileges decide whether RLS is bypassed.
CREATE FUNCTION openrails.assert_cross_merchant_reader() RETURNS void
    LANGUAGE plpgsql STABLE
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
         WHERE rolname = current_user AND (rolsuper OR rolbypassrls)
    ) THEN
        RAISE EXCEPTION
            'openrails: cross-merchant directory lookup requires a definer that bypasses RLS, but %I does not (#824)', current_user
            USING ERRCODE = 'insufficient_privilege',
                  HINT = 'apply migrations as a superuser (or a BYPASSRLS role) so the SECURITY DEFINER directory functions can read across merchants; otherwise webhook routing silently resolves nothing';
    END IF;
END;
$$;

-- Webhook/callback routing + global PSP ownership. (rail, environment,
-- account_id) is a GLOBAL natural key (uq_rail_merchant_accounts_identity), so
-- this is an indexed single-row lookup, not a scan.
CREATE FUNCTION openrails.psp_owner_by_identity(p_rail text, p_environment text, p_account_id text)
    RETURNS TABLE (id uuid, merchant_id uuid, rail text, environment text, account_id text)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT p.id, p.merchant_id, p.rail, p.environment, p.account_id
      FROM openrails.psps p
     WHERE p.rail = lower(p_rail)
       AND p.environment = p_environment
       AND p.account_id = p_account_id
     LIMIT 1;
END;
$$;

COMMENT ON FUNCTION openrails.psp_owner_by_identity(text, text, text) IS
    'Cross-merchant PSP ownership lookup by the GLOBAL (rail, environment, account_id) natural key. The one sanctioned way to answer "which merchant owns this provider account" before a merchant context exists (inbound webhooks, the global-uniqueness preflight). Returns the routing tuple only — no credentials, no listing.';

-- Hosted-portal merchant directory for a subject. Narrow on purpose: it pierces
-- ONLY openrails.customers, and only to yield merchant ids. Everything else
-- (active/deleted filtering, slug/display name) is an ordinary query against
-- the global, policy-free openrails.merchants directory.
CREATE FUNCTION openrails.customer_merchant_ids_for_subject(p_subject text)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT c.merchant_id
      FROM openrails.customers c
     WHERE c.subject = p_subject;
END;
$$;

COMMENT ON FUNCTION openrails.customer_merchant_ids_for_subject(text) IS
    'Merchants where an AuthKit subject holds a customer record, across every merchant scope. For the hosted portal''s "your merchants" list, which runs before any merchant is chosen.';

-- customers' only subject index is (merchant_id, subject); a subject-first
-- directory lookup had no index at all. Not CONCURRENTLY: the migrator applies
-- each file inside ONE transaction, where CREATE INDEX CONCURRENTLY is illegal
-- (same constraint as the five indexes in 0011).
CREATE INDEX idx_customers_subject ON openrails.customers USING btree (subject) WHERE (subject IS NOT NULL);

-- EXECUTE is granted to PUBLIC by default; these are deliberate escape hatches,
-- so name their caller explicitly.
REVOKE ALL ON FUNCTION openrails.current_merchant_id() FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.assert_cross_merchant_reader() FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.psp_owner_by_identity(text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION openrails.customer_merchant_ids_for_subject(text) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION openrails.current_merchant_id() TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.assert_cross_merchant_reader() TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.psp_owner_by_identity(text, text, text) TO openrails_app;
GRANT EXECUTE ON FUNCTION openrails.customer_merchant_ids_for_subject(text) TO openrails_app;
