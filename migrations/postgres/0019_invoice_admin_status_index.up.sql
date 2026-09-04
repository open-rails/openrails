-- Use the existing transactional-migration contention bounds: rollback rather
-- than leave invoice writes queued behind an unexpectedly expensive DDL lock.
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- Merchant invoice administration filters by status and orders within that
-- merchant/status by billing period. Existing invoice indexes lead with the
-- customer and cannot cover the new merchant-wide status filter.
CREATE INDEX ix_invoices_merchant_status_period
    ON openrails.invoices (merchant_id, status, period_from DESC, id DESC);
