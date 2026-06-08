SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- Drop the service_jwt_grants table. Registering an issuer to a tenant is now the
-- authorization on its own: a tenant has full authority over its own resources, so
-- a service JWT's permission claims are authoritative (scoped to the issuer's own
-- tenant) and there is no separate server-side grant to intersect against.
DROP INDEX IF EXISTS billing.service_jwt_grants_lookup_idx;
DROP TABLE IF EXISTS billing.service_jwt_grants;
