-- Repair: none-needed A widened vocabulary: every stored row already satisfies it.
-- Each PSP declares its environment: live moves real money, test is the
-- provider's own system moving none, sandbox the provider's separate sandbox
-- system. A deployment's current PSPs are all live or all non-live.
ALTER TABLE billing.psps DROP CONSTRAINT psps_environment_check;
ALTER TABLE billing.psps ADD CONSTRAINT psps_environment_check CHECK (environment = ANY (ARRAY['live'::text, 'test'::text, 'sandbox'::text]));
COMMENT ON COLUMN billing.psps.environment IS 'The PSP''s declared environment: live (real money), test (the provider''s own system, no money moves) or sandbox (the provider''s separate sandbox system). Part of its identity; a deployment''s current PSPs are all live or all non-live.';
