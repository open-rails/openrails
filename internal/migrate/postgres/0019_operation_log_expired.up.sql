-- An operation dropped because its relevance window passed before it was sent
-- is a phase of its history too: the operations history records it.
ALTER TABLE billing.operation_log DROP CONSTRAINT operation_log_action_check;
ALTER TABLE billing.operation_log ADD CONSTRAINT operation_log_action_check
    CHECK (action IN ('attempting', 'succeeded', 'failed', 'unknown', 'parked', 'expired'));

COMMENT ON COLUMN billing.provider_intents.expires_at IS 'End of the relevance window: an operation never sent expires past this instant instead of firing stale, however often a park or a failure put it back, and the operations history records it (NULL = relevance governed solely by the type''s relevance check).';
