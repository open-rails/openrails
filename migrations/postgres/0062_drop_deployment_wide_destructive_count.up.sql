-- or#887 tail: retire the deployment-wide destructive-intent reader 0028 orphaned.
--
-- 0028 replaced the shared, cross-tenant budget with a PER-MERCHANT one
-- (count_destructive_intents_for_merchant_since) precisely because one budget
-- spanning every merchant is a cross-tenant denial of service. It dropped 0024's
-- system-origin function and index at the time, but 0021's deployment-wide
-- count_destructive_intents_since survived with no caller: the #732 ceiling now
-- reads the per-merchant leg and the per-actor leg, and nothing asks for a
-- global total any more.
--
-- Leaving it in place is not neutral. It is a SECURITY DEFINER cross-merchant
-- reader granted to openrails_app, and the surviving function/index pair reads
-- to the next person as evidence that a deployment-wide wall still exists. It
-- does not, and or#887 decided it must not.
--
-- What STAYS:
--   * count_destructive_intents_by_actor_since — live. One compromised
--     credential operating across merchants is exactly what it must see, and
--     that is a property OF the actor, not a shared budget between tenants.
--   * count_destructive_intents_for_merchant_since (0028) — the ceiling's
--     merchant-scoped reader for both origin legs.
--   * idx_rail_intents_destructive_actor_window — serves the actor leg.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP FUNCTION openrails.count_destructive_intents_since(text[], timestamptz);

-- 0021 built this partial index solely for the function above: leading on
-- created_at with no merchant column, it can serve no other predicate in the
-- codebase. The actor leg has its own (actor, created_at, intent_type) index and
-- the per-merchant leg has 0028's (merchant_id, origin, created_at, intent_type).
DROP INDEX openrails.idx_rail_intents_destructive_window;
