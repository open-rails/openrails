-- =============================================================================
-- OpenRails billing seed data.
--
-- Keep this file limited to production-safe rows required by the schema and
-- application invariants. Environment-specific/dev catalog data does not belong
-- here.
-- =============================================================================

SET lock_timeout = '10s';
SET statement_timeout = '300s';

INSERT INTO openrails.tenants (id, slug, name, status)
VALUES ('00000000-0000-0000-0000-000000000001', 'default', 'Default Tenant', 'active')
ON CONFLICT (slug) DO NOTHING;
