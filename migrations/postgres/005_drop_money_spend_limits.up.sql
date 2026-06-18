-- =============================================================================
-- #513 B1 HARD CUT — drop the dead per-invoker money spend-limit table.
--
-- openrails.money_spend_limits backed the spend-limit feature
-- (CheckSpendAllowed / SetSpendLimit), which had ZERO live callers: no HTTP
-- route, and admitter.Admit never read it. Per-invoker day/month spend caps are
-- now expressed as scoped_spend_caps scope=invoker windows (the unified admission
-- policy), so the table and all its queries are removed outright (pre-launch: no
-- data migration). The Go enforcement, domain model, and sqlc queries are deleted
-- in the same change.
-- =============================================================================

DROP TABLE IF EXISTS openrails.money_spend_limits;
