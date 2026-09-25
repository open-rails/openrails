-- parent: 17 sha256:791ecdf52a98d58ac030b567e5145168a7b5024347cd7fc2fb9adbc71d5c2296
-- #1093 ruling (a): NMI never retries a decline, so every provider-scheduled
-- NMI subscription is dunned by OpenRails. provider and provider_dunning on
-- NMI become one policy, nmi_schedule; provider is left to the mirrors.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.subscriptions
    DROP CONSTRAINT subscriptions_collection_policy_check,
    DROP CONSTRAINT subscriptions_dunning_rail_check;

ALTER TABLE openrails.subscriptions DISABLE TRIGGER subscriptions_collection_policy_immutable;
UPDATE openrails.subscriptions SET collection_policy = 'nmi_schedule'
WHERE rail = 'nmi' AND collection_policy IN ('provider', 'provider_dunning');
ALTER TABLE openrails.subscriptions ENABLE TRIGGER subscriptions_collection_policy_immutable;

ALTER TABLE openrails.subscriptions
    ADD CONSTRAINT subscriptions_collection_policy_check
        CHECK (collection_policy IN ('provider', 'nmi_schedule', 'engine')) NOT VALID,
    ADD CONSTRAINT subscriptions_nmi_schedule_rail_check
        CHECK ((collection_policy = 'nmi_schedule') = (rail = 'nmi' AND collection_policy <> 'engine')) NOT VALID;

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
                AND ((p_include_engine AND s.collection_policy='engine') OR (s.collection_policy='nmi_schedule' AND s.rail='nmi')))
            OR (p_include_engine AND s.collection_policy='engine' AND s.current_period_ends_at <= p_now
                AND (s.status='active' OR (s.status='past_due' AND s.next_retry_at <= p_now))
                AND NOT EXISTS (SELECT 1 FROM openrails.rail_intents i WHERE i.merchant_id=s.merchant_id AND i.subscription_id=s.id AND i.intent_type='subscription_collection' AND i.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable'))))
       AND s.deleted_at IS NULL
     GROUP BY s.merchant_id
     ORDER BY MIN(CASE WHEN s.status='awaiting_method' THEN s.grace_ends_at WHEN s.collection_policy='engine' AND s.status='active' THEN s.current_period_ends_at ELSE s.next_retry_at END), s.merchant_id
     LIMIT p_limit;
END;
$$;
