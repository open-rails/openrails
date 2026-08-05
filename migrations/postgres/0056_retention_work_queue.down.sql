-- Reverting re-instates the hourly full walk of every active merchant. Only do
-- this alongside reverting CleanupExpiredDataWorker.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP FUNCTION IF EXISTS openrails.retention_work_merchant_ids(timestamptz, timestamptz, timestamptz, timestamptz, timestamptz, timestamptz, uuid, int);

-- Restore 0023's arbitrary-truncation dunning fan-out.
CREATE OR REPLACE FUNCTION openrails.due_dunning_merchant_ids(p_rails text[], p_now timestamptz, p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT s.merchant_id
      FROM openrails.subscriptions s
     WHERE s.rail = ANY(p_rails)
       AND s.status = 'past_due'
       AND s.next_retry_at IS NOT NULL AND s.next_retry_at <= p_now
       AND s.deleted_at IS NULL
     LIMIT p_limit;
END;
$$;

DROP TABLE IF EXISTS openrails.worker_sweep_cursors;

DROP INDEX IF EXISTS openrails.ix_checkout_sessions_expirable;
DROP INDEX IF EXISTS openrails.ix_webhook_events_retention;
DROP INDEX IF EXISTS openrails.ix_notification_queue_retention;
