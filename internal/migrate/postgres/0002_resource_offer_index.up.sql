-- parent: 1 sha256:454ac14c0ba2dee1a8aadb904a3c3f5ad3b5ffd982d83464a8dc0755d107cc81
-- Reverse resource-to-offer discovery uses exact opaque entitlement keys.
-- The grant ledger remains the access authority; this is only a catalog index.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';
CREATE INDEX products_active_entitlements_spec
ON openrails.products USING gin (entitlements_spec)
WHERE NOT archived;
