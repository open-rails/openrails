-- parent: 16 sha256:a98b4a6b1fcbdb09db42024e7627e1d69ff071cc8058a2eea5eb06c6415cd8a3
-- An engine renewal whose stored method is unusable (gone, parked, no longer
-- qualified) is due, so admission routes it to awaiting_method; a membership
-- awaiting a method past its dunning window is due to end.
CREATE OR REPLACE FUNCTION openrails.due_dunning_merchant_ids(p_rails text[], p_now timestamp with time zone, p_limit integer, p_include_engine boolean) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT s.merchant_id
      FROM openrails.subscriptions s
     WHERE s.rail = ANY(p_rails)
       AND ((s.collection_policy <> 'engine' AND s.rail='nmi' AND s.status='past_due' AND s.next_retry_at IS NOT NULL AND s.next_retry_at <= p_now)
            OR (s.status='awaiting_method' AND s.grace_ends_at <= p_now
                AND ((p_include_engine AND s.collection_policy='engine') OR (s.collection_policy='provider_dunning' AND s.rail='nmi')))
            OR (p_include_engine AND s.collection_policy='engine' AND s.current_period_ends_at <= p_now
                AND (s.status='active' OR (s.status='past_due' AND s.next_retry_at <= p_now))
                AND NOT EXISTS (SELECT 1 FROM openrails.rail_intents i WHERE i.merchant_id=s.merchant_id AND i.subscription_id=s.id AND i.intent_type='subscription_collection' AND i.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable'))))
       AND s.deleted_at IS NULL
     GROUP BY s.merchant_id
     ORDER BY MIN(CASE WHEN s.status='awaiting_method' THEN s.grace_ends_at WHEN s.collection_policy='engine' AND s.status='active' THEN s.current_period_ends_at ELSE s.next_retry_at END), s.merchant_id
     LIMIT p_limit;
END;
$$;
