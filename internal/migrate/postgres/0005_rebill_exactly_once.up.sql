-- parent: 4 sha256:a02e15767023ac09e277f9013baa5490b9aba28bea8f3386d4efe5c0014d9b96
-- Multi-replica exactly-once rebilling (#1075). Admission serializes on the
-- subscription row; these make the same invariants hold in the database for
-- every writer on every replica: at most one unresolved charge operation per
-- subscription, and one engine operation per (period, attempt) retry slot.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE UNIQUE INDEX uq_rail_intents_open_subscription_collection ON openrails.rail_intents (merchant_id, subscription_id)
WHERE intent_type = 'subscription_collection' AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable');

CREATE UNIQUE INDEX uq_rail_intents_subscription_collection_slot
ON openrails.rail_intents (merchant_id, subscription_id, (payload->>'previous_period_end'), (payload->>'attempt'))
WHERE intent_type = 'subscription_collection';

CREATE UNIQUE INDEX uq_rail_intents_open_manual_rebill ON openrails.rail_intents (merchant_id, subscription_id)
WHERE intent_type = 'manual_rebill' AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable');
