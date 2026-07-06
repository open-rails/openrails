-- #764: openrails_app's grants (migration 0001) stop at the `openrails`
-- schema. A production-shaped boot also needs:
--   - River's job-queue tables (`public`, config.RiverSchema) — every
--     enqueue/dequeue/work-loop iteration runs as openrails_app.
--   - AuthKit's `profiles` schema — the control plane's identity/session
--     tables, likewise queried as openrails_app.
--
-- This runs as step 3 of internal/migrate.RunPostgres, AFTER step 1 (AuthKit
-- migrations, creates `profiles`) and step 2 (River migrations, creates the
-- tables below) — see internal/migrate/migrator.go's fixed ordering comment —
-- so every object granted here already exists.
--
-- Previously this shape existed ONLY in test-only code
-- (internal/dbtest/rls.go's enableAppRoleLogin) and, for openrails-saas, a
-- hand-rolled scripts/staging/db-bootstrap.sql the host had to ship on top of
-- openrails' own migrations. This migration is now the single source of
-- truth; both workarounds are removed once a host is on this version.

-- ---------------------------------------------------------------------------
-- River (public schema). Least-privilege: name the actual tables the runtime
-- client touches (per riverpgxv5's dbsqlc query set) rather than blanket ALL
-- TABLES IN SCHEMA public. river_migration is deliberately excluded — only
-- River's own migrator (running as the admin/migrate role) ever reads or
-- writes it; the runtime client never touches it.
--
-- Guarded on existence: internal/migrate.RunPostgres (the one production
-- path) always creates these before this migration ever runs, but a
-- from-scratch or test-only harness that applies OpenRails' own migrations
-- without also running River's separate migrator must not fail here — the
-- ALTER DEFAULT PRIVILEGES below still covers those tables the moment they
-- (or a later River version's new tables) DO get created by this same role.
GRANT USAGE ON SCHEMA public TO openrails_app;
DO $$
BEGIN
  IF to_regclass('public.river_job') IS NOT NULL THEN
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.river_job TO openrails_app;
  END IF;
  IF to_regclass('public.river_job_id_seq') IS NOT NULL THEN
    GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.river_job_id_seq TO openrails_app;
  END IF;
  IF to_regclass('public.river_queue') IS NOT NULL THEN
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.river_queue TO openrails_app;
  END IF;
  IF to_regclass('public.river_leader') IS NOT NULL THEN
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.river_leader TO openrails_app;
  END IF;
END $$;

-- The migratekit ledger table (public.migrations) predates this migration on
-- every deployment (created at the bootstrap step, before AuthKit/River/
-- OpenRails migrations run), so the default privileges below don't reach it
-- retroactively. The runtime reads it once at boot to confirm migrations are
-- applied (internal/app/build_runtime.go validateDatabase) — read-only, no
-- write path needs it.
GRANT SELECT ON TABLE public.migrations TO openrails_app;

-- A later River version bump's NEW tables must inherit the same grant
-- automatically — both migrators (River's and OpenRails') run as the SAME
-- admin/migrate role that applies this migration, so ALTER DEFAULT PRIVILEGES
-- (scoped to that role, implicitly) covers them without a follow-up migration.
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO openrails_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO openrails_app;

-- ---------------------------------------------------------------------------
-- AuthKit (profiles schema) — a sibling-owned, independently-versioned
-- schema (github.com/open-rails/authkit's own migrations). Unlike River,
-- granted at the SCHEMA boundary rather than by table name: this repo does
-- not own or want to track AuthKit's internal table shape, exactly as
-- openrails.* is granted at the openrails schema boundary in migration 0001.
GRANT USAGE ON SCHEMA profiles TO openrails_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA profiles TO openrails_app;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA profiles TO openrails_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA profiles GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO openrails_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA profiles GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO openrails_app;
