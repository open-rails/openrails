-- =============================================================================
-- 050 — Per-tenant Row Level Security + per-tenant encryption-at-rest keys
--       (issue #227, code-able slices: Postgres RLS + envelope encryption)
--
-- Builds on migration 039 (tenant_id NOT NULL on every tenant-owned table,
-- billing.tenants directory) and 041 (billing.tenant_secrets). Adds the two
-- defence-in-depth layers #227 calls for:
--
--   (A) Postgres ROW LEVEL SECURITY on every tenant-owned table. Rows are
--       restricted to the tenant whose id is set in the per-transaction GUC
--       `app.tenant_id`. The application sets that GUC at the start of each
--       request-scoped transaction (see internal/db: SetTenantGUC /
--       RunInTenantTx). With RLS forced, a missing `WHERE tenant_id = ...`
--       predicate can no longer leak another tenant's rows.
--
--   (B) Per-tenant encryption-at-rest: a table of wrapped (envelope-encrypted)
--       per-tenant Data Encryption Keys (DEKs). A master key (config/env in
--       self-hosted, KMS in production) wraps each tenant's DEK; the DEK
--       encrypts sensitive field values (e.g. billing.tenant_secrets.value).
--
-- PRODUCTION REQUIREMENT (RLS): RLS only ENFORCES when the app connects as a
-- NON-superuser, NON-BYPASSRLS role. This migration creates the unprivileged
-- application role `openrails_app` and grants it DML on the billing schema.
-- Production MUST connect the app as this role (DB_USERNAME=openrails_app).
-- Superusers and the migration-runner role bypass RLS by design (they need to
-- run cross-tenant DDL/migrations); they must NEVER be the request-path role.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- -----------------------------------------------------------------------------
-- (A.1) Unprivileged application role. NOLOGIN here: production attaches a
-- password / LOGIN out of band (or via a secret manager) so the credential is
-- not baked into the migration. Crucially it has NO BYPASSRLS and is NOT a
-- superuser, so the policies below actually constrain it.
-- -----------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'openrails_app') THEN
        CREATE ROLE openrails_app NOLOGIN NOBYPASSRLS;
    END IF;
END $$;

GRANT USAGE ON SCHEMA billing TO openrails_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA billing TO openrails_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA billing TO openrails_app;
-- Future tables/sequences in the billing schema inherit the same DML grants so
-- later migrations do not silently lock the app role out.
ALTER DEFAULT PRIVILEGES IN SCHEMA billing
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO openrails_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA billing
    GRANT USAGE, SELECT ON SEQUENCES TO openrails_app;

-- -----------------------------------------------------------------------------
-- (A.2) Enable + FORCE row level security on every tenant-owned table and add a
-- per-tenant policy. The policy reads the current tenant from the
-- `app.tenant_id` GUC (set per transaction via set_config('app.tenant_id', $1,
-- true)). When the GUC is unset/empty the predicate is NULL -> no rows match, so
-- a query that forgets to set the tenant sees NOTHING rather than everything
-- (fail-closed).
--
-- FORCE ROW LEVEL SECURITY makes the policy apply even to the table OWNER, so an
-- accidentally-privileged connection still cannot bypass it (only superuser /
-- BYPASSRLS roles bypass, which is exactly why the app role has neither).
--
-- This is the SAME tenant-owned table list as migration 039.
-- -----------------------------------------------------------------------------

DO $$
DECLARE
    t TEXT;
    tenant_owned_tables CONSTANT TEXT[] := ARRAY[
        'products',
        'prices',
        'catalog_drift_events',
        'payment_methods',
        'subscriptions',
        'entitlements',
        'payments',
        'admin_grants',
        'notification_queue',
        'processor_customers',
        'credit_types',
        'credit_transactions',
        'credit_blocks',
        'user_credit_balances',
        'checkout_sessions',
        'manual_rebill_attempts'
    ];
BEGIN
    FOREACH t IN ARRAY tenant_owned_tables LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.tables
            WHERE table_schema = 'billing' AND table_name = t
        ) THEN
            EXECUTE format('ALTER TABLE billing.%I ENABLE ROW LEVEL SECURITY', t);
            EXECUTE format('ALTER TABLE billing.%I FORCE  ROW LEVEL SECURITY', t);

            -- Idempotent: drop then (re)create the policy so re-running the
            -- migration converges. One policy covers all commands; both USING
            -- (read/update/delete visibility) and WITH CHECK (insert/update
            -- target) require tenant_id to equal the GUC, so a row can neither be
            -- read nor written outside the active tenant.
            EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON billing.%I', t);
            EXECUTE format($pol$
                CREATE POLICY tenant_isolation ON billing.%I
                    USING       (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
                    WITH CHECK  (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
            $pol$, t);
        END IF;
    END LOOP;
END $$;

-- -----------------------------------------------------------------------------
-- (B) billing.tenant_deks — wrapped (envelope-encrypted) per-tenant Data
-- Encryption Keys. One row per tenant. `wrapped_dek` is the tenant's 256-bit DEK
-- sealed with the master key (AES-256-GCM): nonce || ciphertext || tag. The
-- master key never touches the DB; it comes from config/env (self-hosted) or a
-- KMS (production). Rotating the master key re-wraps these rows; rotating a
-- tenant DEK bumps key_version.
--
-- GLOBAL control-plane table (it IS the per-tenant key directory), like
-- billing.tenants and billing.tenant_secrets — so it is NOT given RLS / a
-- tenant policy here; access is mediated by the crypto package, which only ever
-- reads/writes the row for the requested tenant id.
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.tenant_deks (
    tenant_id    UUID        NOT NULL,
    wrapped_dek  BYTEA       NOT NULL,
    key_version  INTEGER     NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,

    CONSTRAINT pk_tenant_deks PRIMARY KEY (tenant_id)
);

COMMENT ON TABLE billing.tenant_deks IS
    'Wrapped per-tenant Data Encryption Keys for envelope encryption-at-rest (issue #227). wrapped_dek = tenant DEK sealed with the master key (AES-256-GCM, nonce||ct||tag). Master key lives in config/env (self-hosted) or KMS (production), never in the DB. GLOBAL control-plane table.';
COMMENT ON COLUMN billing.tenant_deks.wrapped_dek IS
    'AES-256-GCM(master_key, tenant_dek): nonce(12) || ciphertext(32) || tag(16).';

GRANT SELECT, INSERT, UPDATE, DELETE ON billing.tenant_deks TO openrails_app;
