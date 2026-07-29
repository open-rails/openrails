-- Reverting leaves origin='system' destructive queueing unwalled again. Only do
-- this alongside reverting the caller (intents.RateCeiling).
DROP INDEX IF EXISTS openrails.idx_rail_intents_system_destructive_window;
DROP FUNCTION IF EXISTS openrails.count_system_destructive_intents_for_merchant_since(uuid, text[], timestamptz);
