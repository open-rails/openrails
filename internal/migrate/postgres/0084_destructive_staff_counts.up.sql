-- Repair: none-needed Index change only: admission counts read staff and application actions, never the host's own uncounted work.
DROP INDEX billing.destructive_actions_actor_recent_idx;
DROP INDEX billing.destructive_actions_merchant_recent_idx;
DROP INDEX billing.destructive_actions_recent_idx;
CREATE INDEX destructive_actions_actor_recent_idx ON billing.destructive_actions(actor_key,(greatest(admitted_at,finished_at))) INCLUDE (units) WHERE state='succeeded' AND origin='staff';
CREATE INDEX destructive_actions_merchant_recent_idx ON billing.destructive_actions(merchant_id,origin,(greatest(admitted_at,finished_at))) INCLUDE (units) WHERE state='succeeded';
CREATE INDEX destructive_actions_recent_idx ON billing.destructive_actions((greatest(admitted_at,finished_at))) INCLUDE (units) WHERE state='succeeded' AND origin='staff';
COMMENT ON COLUMN billing.destructive_actions.origin IS 'staff: a staff or application actor, counted and held. host: the embedded host''s own in-process work, audited but never counted or held. automation: named autonomous work on its own budget.';
