-- Reverting restores the or#860 fail-open: the rate ceiling counts 0 and the
-- armed scans select nothing. Only do this alongside reverting the callers.
DROP INDEX IF EXISTS openrails.idx_rail_intents_destructive_actor_window;
DROP INDEX IF EXISTS openrails.idx_rail_intents_destructive_window;
DROP FUNCTION IF EXISTS openrails.armed_findings_digest_merchant_ids();
DROP FUNCTION IF EXISTS openrails.armed_alert_merchant_ids();
DROP FUNCTION IF EXISTS openrails.count_destructive_intents_by_actor_since(text, text[], timestamptz);
DROP FUNCTION IF EXISTS openrails.count_destructive_intents_since(text[], timestamptz);
