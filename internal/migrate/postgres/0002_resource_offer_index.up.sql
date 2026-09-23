-- parent: 1 sha256:4d9c70eea4f34a67cf6a6c9af4c7147057a4bb54fd086916e0190f39a47dc4b9
-- Reverse resource-to-offer discovery uses exact opaque entitlement keys.
-- The grant ledger remains the access authority; this is only a catalog index.
CREATE INDEX products_active_entitlements_spec
ON openrails.products USING gin (entitlements_spec)
WHERE NOT archived;
