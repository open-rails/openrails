-- Repair: none-needed Existing opaque capabilities retain their identity and
-- merchant binding. This non-unique lookup index also lets the resolver refuse
-- any duplicate hash instead of choosing a merchant.
CREATE INDEX checkout_sessions_id_hash_idx ON billing.checkout_sessions (id_hash);
