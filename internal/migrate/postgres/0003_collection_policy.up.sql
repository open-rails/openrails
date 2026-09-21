-- parent: 2 sha256:e970f356c77aa8c7a740f63ad7e159dde7c7ad7465e66fc079f143aa7ec611fd
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '300s';

-- Adopt authority without transferring any existing provider agreement. The
-- legacy card flag remains stored for archive fidelity and staged deployment;
-- current workers use only the immutable subscription policy.
ALTER TABLE openrails.subscriptions ADD COLUMN collection_policy text NOT NULL DEFAULT 'provider';
UPDATE openrails.subscriptions s SET collection_policy = 'provider_dunning'
FROM openrails.payment_methods pm
WHERE s.payment_method_id=pm.id AND s.merchant_id=pm.merchant_id
  AND s.customer_id=pm.customer_id AND s.psp_id=pm.psp_id
  AND s.rail='nmi' AND pm.rail='nmi' AND pm.rebill_driver='openrails';
-- Solana's established delegated-pull sidecar is proof of engine ownership;
-- its external PDA is an execution binding, not a provider card schedule.
UPDATE openrails.subscriptions s SET collection_policy = 'engine'
WHERE s.rail='solana' AND EXISTS (
 SELECT 1 FROM openrails.solana_subscriptions ss
 WHERE ss.merchant_id=s.merchant_id AND ss.subscription_id=s.id);
ALTER TABLE openrails.subscriptions
 ADD CONSTRAINT subscriptions_collection_policy_check CHECK (collection_policy IN ('provider','provider_dunning','engine')),
 ADD CONSTRAINT subscriptions_engine_binding_check CHECK (collection_policy <> 'engine' OR
   ((rail IN ('nmi','stripe') AND rail_subscription_id='') OR rail='solana')),
 ADD CONSTRAINT subscriptions_dunning_rail_check CHECK (collection_policy <> 'provider_dunning' OR rail='nmi');
-- A missing/removed instrument blocks charge admission, not the agreement.
CREATE INDEX idx_subscriptions_engine_due ON openrails.subscriptions (merchant_id,current_period_ends_at,next_retry_at)
 WHERE collection_policy='engine' AND status IN ('active','past_due') AND deleted_at IS NULL;
CREATE INDEX idx_subscriptions_engine_due_global ON openrails.subscriptions (current_period_ends_at,merchant_id)
 WHERE collection_policy='engine' AND status IN ('active','past_due') AND deleted_at IS NULL;

CREATE FUNCTION openrails.preserve_subscription_collection_policy() RETURNS trigger
 LANGUAGE plpgsql SET search_path TO 'openrails','pg_catalog' AS $$
BEGIN
 IF NEW.collection_policy IS DISTINCT FROM OLD.collection_policy THEN
  RAISE EXCEPTION 'subscription collection policy is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER subscriptions_collection_policy_immutable BEFORE UPDATE OF collection_policy
 ON openrails.subscriptions FOR EACH ROW EXECUTE FUNCTION openrails.preserve_subscription_collection_policy();

-- The legacy 3-argument entry remains usable only by policy-aware workers:
-- never hand an engine obligation to the old dunning dispatcher.
CREATE OR REPLACE FUNCTION openrails.due_dunning_merchant_ids(p_rails text[],p_now timestamp with time zone,p_limit integer) RETURNS TABLE(merchant_id uuid)
 LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path TO 'openrails','pg_catalog' AS $$
BEGIN
 RETURN QUERY SELECT s.merchant_id FROM openrails.subscriptions s
 WHERE s.rail=ANY(p_rails) AND s.collection_policy<>'engine' AND s.status='past_due'
 AND s.next_retry_at IS NOT NULL AND s.next_retry_at<=p_now AND s.deleted_at IS NULL
 GROUP BY s.merchant_id ORDER BY MIN(s.next_retry_at) LIMIT p_limit;
END;
$$;

CREATE FUNCTION openrails.due_dunning_merchant_ids(p_rails text[], p_now timestamp with time zone, p_limit integer, p_include_engine boolean) RETURNS TABLE(merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    RETURN QUERY
    SELECT s.merchant_id
      FROM openrails.subscriptions s
     WHERE s.rail = ANY(p_rails)
       AND ((s.collection_policy <> 'engine' AND s.status='past_due' AND s.next_retry_at IS NOT NULL AND s.next_retry_at <= p_now)
            OR (p_include_engine AND s.collection_policy='engine' AND s.current_period_ends_at <= p_now
                AND (s.status='active' OR (s.status='past_due' AND s.next_retry_at <= p_now))
                AND EXISTS (SELECT 1 FROM openrails.payment_methods pm JOIN openrails.psps p ON p.id=pm.psp_id AND p.merchant_id=pm.merchant_id LEFT JOIN openrails.custodians c ON c.id=pm.custodian_id AND c.merchant_id=pm.merchant_id
                            WHERE pm.id=s.payment_method_id AND pm.merchant_id=s.merchant_id AND pm.customer_id=s.customer_id AND pm.psp_id=s.psp_id
                              AND pm.park_reason='' AND pm.stored_credential_recurring_ref<>'' AND NOT p.archived
                         AND ((pm.custodian='hyperswitch' AND pm.rail='nmi' AND NOT c.archived AND p.environment=c.environment)
                              OR (pm.custodian='psp' AND pm.rail IN ('nmi','stripe') AND pm.rail_customer_ref<>'' AND pm.rail_method_ref<>'')))
                AND NOT EXISTS (SELECT 1 FROM openrails.rail_intents i WHERE i.merchant_id=s.merchant_id AND i.subscription_id=s.id AND i.intent_type='subscription_collection' AND i.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable'))))
       AND s.deleted_at IS NULL
     GROUP BY s.merchant_id
     ORDER BY MIN(CASE WHEN s.collection_policy='engine' AND s.status='active' THEN s.current_period_ends_at ELSE s.next_retry_at END), s.merchant_id
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.due_dunning_merchant_ids(p_rails text[], p_now timestamp with time zone, p_limit integer, p_include_engine boolean) IS 'Merchants with a due past_due subscription on the named rails — the fan-out list for DunningWorker. Ids only; the due rows, the charges and every lifecycle transition run per-merchant under RunInMerchantScope. Replaces a bare-context scan that returned an empty slice on every run, so scheduled dunning (retries, #839 staleness parking, #840 terminal handling) never fired at all (or#877 B5).';

REVOKE ALL ON FUNCTION openrails.due_dunning_merchant_ids(p_rails text[], p_now timestamp with time zone, p_limit integer, p_include_engine boolean) FROM PUBLIC;

