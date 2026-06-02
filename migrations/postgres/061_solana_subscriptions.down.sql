-- Down migration for 061_solana_subscriptions (documentation-only; migratekit
-- applies only *.up.sql, matching this repo's convention).
DROP TABLE IF EXISTS billing.solana_subscriptions;
