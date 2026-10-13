-- Repair: none-needed Solana rows are renamed in place (live to mainnet, test and sandbox to devnet); ids derive from the money class, which the rename keeps.
-- A PSP's environment is its rail's own: Solana's are its networks (mainnet,
-- devnet, testnet), the card rails keep live, test and sandbox. live records
-- the money class, which a PSP's identity and the deployment rule use.
ALTER TABLE billing.psps DROP CONSTRAINT psps_environment_check;
DROP INDEX billing.psps_rail_class_account_id_key;
UPDATE billing.psps SET environment = CASE environment WHEN 'live' THEN 'mainnet' ELSE 'devnet' END WHERE rail = 'solana';
ALTER TABLE billing.psps ADD CONSTRAINT psps_environment_check CHECK (CASE WHEN rail = 'solana' THEN environment = ANY (ARRAY['mainnet'::text, 'devnet'::text, 'testnet'::text]) ELSE environment = ANY (ARRAY['live'::text, 'test'::text, 'sandbox'::text]) END);
ALTER TABLE billing.psps ADD COLUMN live boolean NOT NULL GENERATED ALWAYS AS (environment = ANY (ARRAY['live'::text, 'mainnet'::text])) STORED;
CREATE UNIQUE INDEX psps_rail_live_account_id_key ON billing.psps USING btree (rail, live, account_id);
COMMENT ON COLUMN billing.psps.environment IS 'The PSP''s declared environment, its rail''s own: nmi live, test or sandbox; stripe live or test; ccbill live or sandbox; solana mainnet, devnet or testnet.';
COMMENT ON COLUMN billing.psps.live IS 'The PSP''s money class: whether its environment moves real money (live, mainnet). A PSP''s identity is (rail, live, account_id); a sandbox merchant''s PSPs are non-live, a live merchant''s live.';
