-- The merchant's write posture alone decides its provider writes; the
-- deployment-wide provider_write_mode is gone.
COMMENT ON TABLE billing.merchant_write_posture IS 'One merchant''s provider write posture; no row is full. A copy of the billing book is readonly whatever it says. Export sets the source readonly and restore the destination; an operator arms it back to full.';
COMMENT ON COLUMN billing.provider_intents.origin IS 'Who wanted this mutation: user/admin-origin intents execute under a limited write posture (reactive completion), system-origin intents require full. Nothing executes under readonly.';
