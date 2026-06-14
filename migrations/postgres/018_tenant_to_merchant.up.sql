--
-- Rename tenant -> MERCHANT (#480/#481/#482): the OpenRails billing-side "tenant"
-- is a dumb billing bucket, not an identity. This migrates EXISTING databases to
-- the merchant naming the consolidated 001 baseline now declares. It is fully
-- idempotent (every step is guarded), so it is a no-op on a fresh DB whose 001
-- already created the merchant_* objects.
--
--   * tenant_* control-plane tables  -> merchant_* (subjects/deks/secrets/exports/audit)
--   * tenant_id / tenant_subject_id columns -> merchant_id / merchant_subject_id (all tables)
--   * GUC app.tenant_id              -> app.merchant_id (RLS policies + column defaults)
--   * DROP openrails.tenant_delegated_issuers (JWKS = auth, not billing; #480)
--   * DROP merchants.authkit_tenant_id/_slug identity-equation; ADD owner_tenant_id (#481)
--   * admin_grants -> entitlement_grants (#482)
--   * constraint/index names carrying "tenant" -> "merchant"
--
SET statement_timeout = '300s';
SET lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- 1. Table renames (control-plane tables) + admin_grants -> entitlement_grants.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF to_regclass('openrails.tenants') IS NOT NULL THEN
        ALTER TABLE openrails.tenants RENAME TO merchants;
    END IF;
    IF to_regclass('openrails.tenant_subjects') IS NOT NULL THEN
        ALTER TABLE openrails.tenant_subjects RENAME TO merchant_subjects;
    END IF;
    IF to_regclass('openrails.tenant_deks') IS NOT NULL THEN
        ALTER TABLE openrails.tenant_deks RENAME TO merchant_deks;
    END IF;
    IF to_regclass('openrails.tenant_secrets') IS NOT NULL THEN
        ALTER TABLE openrails.tenant_secrets RENAME TO merchant_secrets;
    END IF;
    IF to_regclass('openrails.tenant_exports') IS NOT NULL THEN
        ALTER TABLE openrails.tenant_exports RENAME TO merchant_exports;
    END IF;
    IF to_regclass('openrails.tenant_credential_audit') IS NOT NULL THEN
        ALTER TABLE openrails.tenant_credential_audit RENAME TO merchant_credential_audit;
    END IF;
    IF to_regclass('openrails.admin_grants') IS NOT NULL THEN
        ALTER TABLE openrails.admin_grants RENAME TO entitlement_grants;
    END IF;
END $$;

-- DROP the JWKS/issuer registry — auth lives host-side (embedded) / AuthKit-side
-- (standalone), never as OpenRails billing state (#480).
DROP TABLE IF EXISTS openrails.tenant_delegated_issuers;

-- ---------------------------------------------------------------------------
-- 2. merchants: drop the authkit identity-equation, add the ownership link (#481).
-- ---------------------------------------------------------------------------
DROP INDEX IF EXISTS openrails.uq_tenants_authkit_tenant_id;
ALTER TABLE openrails.merchants DROP COLUMN IF EXISTS authkit_tenant_id;
ALTER TABLE openrails.merchants DROP COLUMN IF EXISTS authkit_tenant_slug;
ALTER TABLE openrails.merchants ADD COLUMN IF NOT EXISTS owner_tenant_id text;
CREATE INDEX IF NOT EXISTS idx_merchants_owner_tenant_id
    ON openrails.merchants USING btree (owner_tenant_id) WHERE (owner_tenant_id IS NOT NULL);
COMMENT ON COLUMN openrails.merchants.owner_tenant_id IS 'OWNERSHIP (not identity) FK to the AuthKit tenant (org) that owns/administers this merchant (#481). NULL in embedded (no AuthKit). One tenant owns MANY merchants; never used to resolve a merchant from a token.';

-- ---------------------------------------------------------------------------
-- 3. Column renames: tenant_id -> merchant_id, tenant_subject_id -> merchant_subject_id
--    across every openrails table that still carries the old column name.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    r record;
BEGIN
    -- tenant_id / tenant_subject_id (billing scope) + target_tenant_id (audited
    -- merchant). owner_tenant_id (AuthKit org ownership, #481) is NOT in this set.
    FOR r IN
        SELECT c.table_name, c.column_name,
               replace(c.column_name, 'tenant', 'merchant') AS new_name
          FROM information_schema.columns c
         WHERE c.table_schema = 'openrails'
           AND c.column_name IN ('tenant_id', 'tenant_subject_id', 'target_tenant_id')
    LOOP
        EXECUTE format('ALTER TABLE openrails.%I RENAME COLUMN %I TO %I',
                       r.table_name, r.column_name, r.new_name);
    END LOOP;
END $$;

-- ---------------------------------------------------------------------------
-- 4. RLS policies: replace tenant_isolation (app.tenant_id) with merchant_isolation
--    (app.merchant_id) on every table that had it.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT tablename
          FROM pg_policies
         WHERE schemaname = 'openrails' AND policyname = 'tenant_isolation'
    LOOP
        EXECUTE format('DROP POLICY tenant_isolation ON openrails.%I', r.tablename);
        EXECUTE format(
            'CREATE POLICY merchant_isolation ON openrails.%I '
            || 'USING ((merchant_id = (NULLIF(current_setting(''app.merchant_id''::text, true), ''''::text))::uuid)) '
            || 'WITH CHECK ((merchant_id = (NULLIF(current_setting(''app.merchant_id''::text, true), ''''::text))::uuid))',
            r.tablename);
    END LOOP;
END $$;

-- Column DEFAULTs that read the old GUC (budget_policies/tier_schedules merchant_id).
DO $$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT c.table_name
          FROM information_schema.columns c
         WHERE c.table_schema = 'openrails'
           AND c.column_name = 'merchant_id'
           AND c.column_default LIKE '%app.tenant_id%'
    LOOP
        EXECUTE format(
            'ALTER TABLE openrails.%I ALTER COLUMN merchant_id SET DEFAULT '
            || 'COALESCE((NULLIF(current_setting(''app.merchant_id''::text, true), ''''::text))::uuid, '
            || '''00000000-0000-0000-0000-000000000001''::uuid)',
            r.table_name);
    END LOOP;
END $$;

-- ---------------------------------------------------------------------------
-- 5. Rename constraints + indexes whose names still carry "tenant".
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    r record;
BEGIN
    -- entitlement_grants (was admin_grants, #482) keeps admin_grants-named
    -- constraints/indexes after the table rename; re-prefix them.
    FOR r IN
        SELECT con.conname
          FROM pg_constraint con
          JOIN pg_class rel ON rel.oid = con.conrelid
          JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
         WHERE nsp.nspname = 'openrails' AND rel.relname = 'entitlement_grants'
           AND con.conname LIKE 'admin_grants%'
    LOOP
        EXECUTE format('ALTER TABLE openrails.entitlement_grants RENAME CONSTRAINT %I TO %I',
                       r.conname, replace(r.conname, 'admin_grants', 'entitlement_grants'));
    END LOOP;
    FOR r IN
        SELECT c.relname AS idxname
          FROM pg_class c
          JOIN pg_namespace nsp ON nsp.oid = c.relnamespace
         WHERE nsp.nspname = 'openrails' AND c.relkind = 'i'
           AND c.relname LIKE 'admin_grants%'
    LOOP
        EXECUTE format('ALTER INDEX openrails.%I RENAME TO %I',
                       r.idxname, replace(r.idxname, 'admin_grants', 'entitlement_grants'));
    END LOOP;

    -- table/PK/FK/CHECK/UNIQUE constraints. owner_tenant_id names the AuthKit org
    -- (ownership, #481) and stays "tenant" — exclude it from the rename.
    FOR r IN
        SELECT con.conname, rel.relname AS table_name
          FROM pg_constraint con
          JOIN pg_class rel ON rel.oid = con.conrelid
          JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
         WHERE nsp.nspname = 'openrails' AND con.conname LIKE '%tenant%'
           AND con.conname NOT LIKE '%owner_tenant%'
    LOOP
        EXECUTE format('ALTER TABLE openrails.%I RENAME CONSTRAINT %I TO %I',
                       r.table_name, r.conname, replace(r.conname, 'tenant', 'merchant'));
    END LOOP;

    -- indexes (incl. the partial/expression ones not backing a constraint)
    FOR r IN
        SELECT c.relname AS idxname
          FROM pg_class c
          JOIN pg_namespace nsp ON nsp.oid = c.relnamespace
         WHERE nsp.nspname = 'openrails' AND c.relkind = 'i'
           AND c.relname LIKE '%tenant%'
           AND c.relname NOT LIKE '%owner_tenant%'
           AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = c.oid)
    LOOP
        EXECUTE format('ALTER INDEX openrails.%I RENAME TO %I',
                       r.idxname, replace(r.idxname, 'tenant', 'merchant'));
    END LOOP;
END $$;
