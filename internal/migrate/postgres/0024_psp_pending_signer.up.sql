-- parent: 23 sha256:8eff7545f44b6b415184ba9dd387d3617c2f46fecf98b818dfac214896ec2870
-- #1101: a Vault Transit signer whose key no longer matches the Solana
-- identity stored for it records the unapproved public key here, and the
-- Solana rail refuses until an operator approves it. Only that approval clears
-- the column; the generic PSP upsert never writes it, so a re-applied manifest
-- or restart cannot silently accept a changed key.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.psps ADD COLUMN pending_signer_public_key text;
