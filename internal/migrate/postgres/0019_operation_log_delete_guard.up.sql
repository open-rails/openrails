-- The operations history refused update and truncate but took any delete. It
-- now refuses every delete but retention's, past its period, and a merchant
-- purge's, in the transaction that opened the purge's run.
CREATE FUNCTION billing.guard_operation_log_delete() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF current_setting('openrails.retention_table', true) IS NOT DISTINCT FROM 'operation_log'
       AND OLD.occurred_at < now() - interval '761 days' THEN
        RETURN OLD;
    END IF;
    IF EXISTS (
        SELECT 1 FROM billing.maintenance_runs r
         WHERE r.merchant_id = OLD.merchant_id AND r.kind = 'merchant_purge' AND r.status = 'running'
    ) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'operation_log rows are deleted only by retention, 761 days after occurred_at, or by a merchant purge' USING ERRCODE='23514';
END;
$$;
COMMENT ON FUNCTION billing.guard_operation_log_delete() IS 'Refuses every DELETE on operation_log except the retention sweep''s (openrails.retention_table names it and the row is 761 days past occurred_at) and a merchant purge''s (its merchant_purge run is running).';
REVOKE ALL ON FUNCTION billing.guard_operation_log_delete() FROM PUBLIC;

CREATE TRIGGER retained_operation_log BEFORE DELETE ON billing.operation_log
    FOR EACH ROW EXECUTE FUNCTION billing.guard_operation_log_delete();

COMMENT ON TABLE billing.operation_log IS 'Operations history: each phase of each provider operation OpenRails ran (attempting before the provider call, then its result or a hold), who wanted it and where it was sent. Attempting and result entries are written ahead, on their own, and never rolled back; a hold commits with the intent''s transition. Append-only: update and truncate are refused, and a delete is refused unless retention or a merchant purge makes it (billing.guard_operation_log_delete). Retention: rows are deleted 25 months (761 days) after occurred_at.';
