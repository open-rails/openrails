SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '60s';

CREATE FUNCTION openrails.pending_merchant_secret_cleanups(p_after uuid,p_limit integer)
RETURNS TABLE(merchant_id uuid,run_id uuid)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path TO 'openrails','pg_catalog'
AS $$
BEGIN
 PERFORM openrails.assert_cross_merchant_reader();
 RETURN QUERY SELECT r.merchant_id,r.id FROM openrails.destructive_runs r
 JOIN openrails.merchants m ON m.id=r.merchant_id
 WHERE r.kind='merchant_purge' AND r.status IN ('running','failed')
   AND r.affected->>'database_purged'='true' AND r.coverage ? 'secret_cleanup'
   AND m.deleted_at IS NOT NULL AND (p_after IS NULL OR r.id>p_after)
 ORDER BY r.id LIMIT p_limit;
END;
$$;
REVOKE ALL ON FUNCTION openrails.pending_merchant_secret_cleanups(uuid,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION openrails.pending_merchant_secret_cleanups(uuid,integer) TO openrails_app;
CREATE INDEX destructive_runs_pending_secret_cleanup_idx ON openrails.destructive_runs(id)
 WHERE kind='merchant_purge' AND status IN ('running','failed')
 AND affected->>'database_purged'='true' AND coverage ? 'secret_cleanup';
