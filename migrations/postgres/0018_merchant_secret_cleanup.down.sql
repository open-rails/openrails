SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '60s';
DROP FUNCTION openrails.pending_merchant_secret_cleanups(uuid,integer);
DROP INDEX openrails.destructive_runs_pending_secret_cleanup_idx;
