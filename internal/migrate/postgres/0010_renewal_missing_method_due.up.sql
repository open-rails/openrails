-- parent: 9 sha256:d69570014cec6f6d1cd74b3fe6ea4527b35dcf546dcc52c05ff955b3431c1552
-- An engine subscription whose stored payment method is gone is due like any
-- other, so the due pass refuses its renewal with a recorded reason
-- (life.due_pass.refused) instead of skipping it silently. Renewals never fall
-- back to the customer's default card (#1087).
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
            OR (p_include_engine AND s.collection_policy='engine' AND s.current_period_ends_at <= p_now
                AND (s.status='active' OR (s.status='past_due' AND s.next_retry_at <= p_now))
                AND (s.payment_method_id IS NULL OR EXISTS (SELECT 1 FROM openrails.payment_methods pm JOIN openrails.psps p ON p.id=pm.psp_id AND p.merchant_id=pm.merchant_id LEFT JOIN openrails.custodians c ON c.id=pm.custodian_id AND c.merchant_id=pm.merchant_id
                            WHERE pm.id=s.payment_method_id AND pm.merchant_id=s.merchant_id AND pm.customer_id=s.customer_id AND pm.psp_id=s.psp_id
                              AND pm.park_reason='' AND pm.stored_credential_recurring_ref<>'' AND NOT p.archived
                         AND ((pm.custodian='hyperswitch' AND pm.rail='nmi' AND NOT c.archived AND p.environment=c.environment)
                              OR (pm.custodian='psp' AND pm.rail IN ('nmi','stripe') AND pm.rail_customer_ref<>'' AND pm.rail_method_ref<>''))))
                AND NOT EXISTS (SELECT 1 FROM openrails.rail_intents i WHERE i.merchant_id=s.merchant_id AND i.subscription_id=s.id AND i.intent_type='subscription_collection' AND i.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable'))))
       AND s.deleted_at IS NULL
     GROUP BY s.merchant_id
     ORDER BY MIN(CASE WHEN s.collection_policy='engine' AND s.status='active' THEN s.current_period_ends_at ELSE s.next_retry_at END), s.merchant_id
     LIMIT p_limit;
END;
$$;
