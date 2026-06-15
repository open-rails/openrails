-- #491 follow-up (owner 2026-06-15): finish the actor -> invoker_id rename, add an
-- invoker_type discriminator, and index the invoker for per-invoker queries.
--
-- The invoker is an OPAQUE, STABLE id (a polymorphic principal: native-user |
-- delegated-user | service-token | issuer). It is NOT a foreign key — standalone
-- openrails's DB needn't hold the principal's record (it lives in the merchant's
-- system), and the principal is polymorphic across four authkit tables. The column
-- is named invoker_id (an id, even without an FK). invoker_type disambiguates the
-- principal KIND (and, when co-located, which authkit table to resolve against).
--
-- Renamed on all 5 invoker-bearing tables; invoker_type added to the 3 attribution
-- tables (usage_events / money_transactions / money_spend_limits) where "who" is
-- resolved (budget_* key the invoker opaquely for windowing, no type needed).
-- RENAME COLUMN auto-updates the dependent UNIQUE/indexes that referenced `actor`.
-- Plain DDL (not a DO/EXECUTE block) so sqlc's static catalog sees the rename;
-- migratekit applies each migration ONCE by name, so non-idempotent renames are safe.

SET lock_timeout = '10s';
SET statement_timeout = '300s';

ALTER TABLE openrails.usage_events        RENAME COLUMN actor TO invoker_id;
ALTER TABLE openrails.money_transactions  RENAME COLUMN actor TO invoker_id;
ALTER TABLE openrails.money_spend_limits  RENAME COLUMN actor TO invoker_id;
ALTER TABLE openrails.budget_window_state RENAME COLUMN actor TO invoker_id;
ALTER TABLE openrails.budget_reservations RENAME COLUMN actor TO invoker_id;

-- invoker_type discriminator on the attribution tables (nullable; merchant-supplied)
ALTER TABLE openrails.usage_events       ADD COLUMN IF NOT EXISTS invoker_type text;
ALTER TABLE openrails.money_transactions ADD COLUMN IF NOT EXISTS invoker_type text;
ALTER TABLE openrails.money_spend_limits ADD COLUMN IF NOT EXISTS invoker_type text;

-- per-invoker query index on usage_events (money_transactions already has an
-- invoker index via the renamed idx_money_transactions_payer_actor).
CREATE INDEX IF NOT EXISTS idx_usage_events_invoker ON openrails.usage_events USING btree (merchant_id, invoker_id, occurred_at DESC);
