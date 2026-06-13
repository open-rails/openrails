--
-- Backfill openrails.tenants.authkit_tenant_id from the embedded AuthKit
-- directory (profiles.tenants) where it is derivable by slug.
--
-- The fleet is migrating to the IMMUTABLE AuthKit tenant uuid as the
-- canonical cross-service identifier; mutable slugs become presentation-only.
-- Service-token tenant resolution (controlplane.tenantForAuthKitTenant) now
-- prefers authkit_tenant_id and only falls back to the mutable slug for
-- legacy credentials or rows not yet linked by uuid — so link every row we
-- can. Rows whose slug has no live AuthKit tenant stay NULL and continue to
-- resolve through the slug fallback.
--
-- AuthKit migrations (profiles schema) run BEFORE billing migrations in
-- internal/migrate.RunPostgres, so profiles.tenants exists here; guard with
-- to_regclass anyway for deployments whose AuthKit state lives elsewhere.
--
-- This is a one-shot, idempotent backfill (authkit_tenant_id IS NULL rows
-- only). New rows are dual-written with id+slug at provision/bootstrap time.
DO $$
BEGIN
  IF to_regclass('profiles.tenants') IS NULL THEN
    RETURN;
  END IF;

  UPDATE openrails.tenants AS bt
     SET authkit_tenant_id = pt.id::text,
         updated_at        = current_timestamp
    FROM profiles.tenants AS pt
   WHERE bt.authkit_tenant_id IS NULL
     AND bt.deleted_at IS NULL
     AND bt.authkit_tenant_slug IS NOT NULL
     AND lower(bt.authkit_tenant_slug) = pt.slug
     AND pt.deleted_at IS NULL
     -- uq_tenants_authkit_tenant_id: never link a uuid that another row
     -- already carries.
     AND NOT EXISTS (
           SELECT 1 FROM openrails.tenants x
            WHERE x.authkit_tenant_id = pt.id::text
         )
     -- authkit_tenant_slug has no unique index: only backfill a slug that
     -- maps to exactly one un-linked row (ambiguous slugs stay NULL and keep
     -- resolving via the slug fallback).
     AND NOT EXISTS (
           SELECT 1 FROM openrails.tenants y
            WHERE y.id <> bt.id
              AND y.authkit_tenant_id IS NULL
              AND y.deleted_at IS NULL
              AND lower(y.authkit_tenant_slug) = pt.slug
         );
END
$$;
