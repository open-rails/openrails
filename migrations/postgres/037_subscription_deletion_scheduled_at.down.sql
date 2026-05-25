-- Reverse of 037: drop the deferred-delete schedule column.
-- NOTE: migratekit (LoadFromFS) only applies *.up.sql files; this .down.sql is
-- kept for documentation / manual rollback and is not auto-loaded.
ALTER TABLE billing.subscriptions
    DROP COLUMN IF EXISTS deletion_scheduled_at;
