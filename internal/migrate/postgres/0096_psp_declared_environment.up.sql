-- Repair: none-needed A widened vocabulary and a coarser unique key: every stored row already satisfies both; existing rows keep test until their next declaration corrects them.
-- Each PSP declares its environment: live moves real money, test is the
-- provider's own system moving none, sandbox the provider's separate sandbox
-- system. Its identity is (rail, money class, account): test and sandbox are
-- one class, so a declaration moving between them corrects the stored
-- environment in place. A deployment's current PSPs are all live or all
-- non-live.
ALTER TABLE billing.psps DROP CONSTRAINT psps_environment_check;
ALTER TABLE billing.psps ADD CONSTRAINT psps_environment_check CHECK (environment = ANY (ARRAY['live'::text, 'test'::text, 'sandbox'::text]));
DROP INDEX billing.psps_rail_environment_account_id_key;
CREATE UNIQUE INDEX psps_rail_class_account_id_key ON billing.psps USING btree (rail, (environment = 'live'), account_id);
COMMENT ON COLUMN billing.psps.environment IS 'The PSP''s declared environment: live (real money), test (the provider''s own system, no money moves) or sandbox (the provider''s separate sandbox system). Its money class (live, or test and sandbox together) is part of its identity; a deployment''s current PSPs are all live or all non-live.';
