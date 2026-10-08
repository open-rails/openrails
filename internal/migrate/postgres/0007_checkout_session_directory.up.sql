-- parent: 6 sha256:acdc4b0704323a20191694da433c0a0671987a06ee2bac2a2b4cb33da8ed991f
-- Repair: none-needed Existing opaque capabilities retain their identity and
-- merchant binding. This non-unique lookup index also lets the resolver refuse
-- any duplicate hash instead of choosing a merchant.
CREATE INDEX checkout_sessions_id_hash_idx ON billing.checkout_sessions (id_hash);
