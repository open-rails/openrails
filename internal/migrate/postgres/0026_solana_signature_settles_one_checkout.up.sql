-- parent: 25 sha256:5bdbf833acba91a8c7d5669b5ea0a5add3a599d6e67d6e4c53bef079815b37ac
-- A landed Solana transaction settles at most one checkout, across every
-- merchant and PSP. The per-(merchant, PSP) unique index could not stop one
-- transfer carrying two checkouts' references from settling both when their
-- PSPs share a payout wallet. A signature is unique on-chain, so the index is
-- install-wide.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE UNIQUE INDEX uq_checkout_sessions_solana_signature ON openrails.checkout_sessions USING btree (transaction_id)
WHERE rail = 'solana' AND transaction_id IS NOT NULL;
