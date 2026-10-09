-- parent: 16 sha256:d72d9edac1c019f173dcef6091f7bd7850f1c5693aa8d620ac4e2065419cb9dd
-- Repair: none-needed A non-unique index refuses no stored row.
CREATE INDEX entitlements_customer_keyspace_idx ON billing.entitlements
  (merchant_id, customer_id, entitlement COLLATE "C") INCLUDE (starts_at, ends_at)
  WHERE revoked_at IS NULL AND deleted_at IS NULL;
COMMENT ON INDEX billing.entitlements_customer_keyspace_idx IS 'Byte-range reads of one customer''s keys under a prefix (CheckEntitlements prefixes).';
