-- #589: drop the vestigial payment_methods.failure_reason denorm.
--
-- It was a display-only denorm: the only runtime writers ever set it to NULL, no
-- charge path wrote a real failure into it, and the one charge precondition that
-- read it was therefore dead. Per-method health (last charge + outcome) is now
-- DERIVED at query time from openrails.payments via the subscription link
-- (#589 listing API); durable failure HISTORY lives in the append-only stores
-- (external_provider_mutation_logs / ClickHouse payment_events), never in this
-- denorm and never in provider_intents (a transient outbox).
ALTER TABLE openrails.payment_methods DROP COLUMN IF EXISTS failure_reason;
