-- #535: support the REVERSE entitlement lookup (ListCustomersWithEntitlement —
-- "which customers hold an active window of entitlement X", ordered by
-- customer_id for keyset pagination). RLS injects a merchant_id predicate, so the
-- covering index leads with merchant_id, then entitlement, then customer_id (the
-- ORDER BY / keyset column). Partial on the live predicate to match the query and
-- stay small. Without it, a reverse-by-name scan has no covering index (only
-- idx_entitlements_merchant_id + source-specific live indexes exist).
CREATE INDEX IF NOT EXISTS idx_entitlements_reverse_active
    ON openrails.entitlements USING btree (merchant_id, entitlement, customer_id)
    WHERE ((revoked_at IS NULL) AND (deleted_at IS NULL));
