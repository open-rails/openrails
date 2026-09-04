-- Use the existing transactional-migration contention bounds: rollback rather
-- than leave invoice writes queued behind an unexpectedly expensive DDL lock.
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP INDEX openrails.ix_invoices_merchant_status_period;
