--
-- Rename the budget SCOPE value 'actor' -> 'invoker' (#491, slice 2). The
-- per-end-user abuse-attribution scope is the INVOKER (merchant-supplied opaque
-- label), not an authenticated actor. budget_policies.scope is STORED data + a
-- wire value consumers send, so this:
--   1. widens the scope CHECK to accept BOTH 'invoker' and (transiently) 'actor',
--   2. migrates existing stored scope='actor' rows to 'invoker'.
-- The reservation/window-state KEY column ('actor') and the money/usage `actor`
-- columns are UNCHANGED (opaque keys; renaming them is out of scope + risky).
-- Reads still accept 'actor' (budgets.NormalizeScope) for any in-flight value.
--
-- Idempotent: re-running is a no-op (constraint reshape is guarded; the UPDATE
-- only touches remaining 'actor' rows).
--
SET statement_timeout = '300s';
SET lock_timeout = '10s';

-- Widen the scope CHECK to accept 'invoker' (keep 'actor' transiently).
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint con
          JOIN pg_class rel ON rel.oid = con.conrelid
          JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
         WHERE nsp.nspname = 'openrails' AND rel.relname = 'budget_policies'
           AND con.conname = 'budget_policies_scope_check'
    ) THEN
        ALTER TABLE openrails.budget_policies DROP CONSTRAINT budget_policies_scope_check;
    END IF;
    ALTER TABLE openrails.budget_policies
        ADD CONSTRAINT budget_policies_scope_check
        CHECK (scope = ANY (ARRAY['subject'::text, 'invoker'::text, 'role'::text, 'actor'::text]));
END $$;

-- Migrate stored data: scope='actor' -> 'invoker'. The UNIQUE key includes
-- scope, so only migrate rows whose 'invoker' twin does not already exist.
UPDATE openrails.budget_policies a SET scope = 'invoker'
 WHERE a.scope = 'actor'
   AND NOT EXISTS (
        SELECT 1 FROM openrails.budget_policies b
         WHERE b.merchant_id = a.merchant_id AND b.customer_id = a.customer_id
           AND b.owner = a.owner AND b.scope_key = a.scope_key
           AND b.scope = 'invoker'
   );

COMMENT ON COLUMN openrails.budget_policies.scope_key IS 'Immutable scope discriminator: role uuid (scope=role) or invoker string (scope=invoker); empty for scope=subject. Never a slug/name.';
