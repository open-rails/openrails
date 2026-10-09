-- parent: 26 sha256:961eabcd68a5888c75a63262f36ec1f53dd11d574c42129fb6ca365a26a4210a
-- Repair: none-needed Every writer admits a later attempt for a period only
-- after the earlier one is released; a stored pair that breaks the index is a
-- period charged twice, and the index refuses to start on it rather than hide it.

-- At most one attempt to pay a subscription period that is not released:
-- every renewal collection and rebill attempt of one period shares the
-- period's order reference, and a later attempt is admitted only once the
-- earlier one failed without moving money.
CREATE UNIQUE INDEX provider_intents_obligation_unreleased_key ON billing.provider_intents
    (merchant_id, subscription_id, (payload->>'order_reference'))
    WHERE intent_type IN ('subscription_collection', 'manual_rebill')
      AND status NOT IN ('failed_terminal', 'superseded', 'expired');
