-- =============================================================================
-- OpenRails billing seed data.
--
-- Keep this file limited to production-safe rows required by the schema and
-- application invariants. Environment-specific/dev catalog data does not belong
-- here.
-- =============================================================================

SET lock_timeout = '10s';
SET statement_timeout = '300s';

-- #336: there is no "default tenant". Tenants are created explicitly — by the
-- control plane (SaaS), the bootstrap manifest (embedded hosts; e.g. doujins
-- resolves slug 'doujins'), or the test harness (dbtest.EnsureTestTenant). No
-- tenant row is seeded. An existing '…0001'/'default' row from an older deploy
-- is left untouched here; re-attributing or removing default-tenant data is a
-- separate operational migration, not seed data.
