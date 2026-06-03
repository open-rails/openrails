-- Down migration for 062_entitlement_features (documentation-only; migratekit
-- applies only *.up.sql, matching this repo's convention).
DROP TABLE IF EXISTS billing.product_entitlement_features;
DROP TABLE IF EXISTS billing.entitlement_features;
