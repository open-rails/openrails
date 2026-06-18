-- =============================================================================
-- #513 B2 HARD CUT — drop the prepaid money_windows reservation table.
--
-- money_windows backed the prepaid credit-window API (open/settle/refill/close,
-- #335): one bulk held reservation a host admitted requests against locally,
-- settled in cross-payer batches. It is superseded by the #513 Redis admission
-- gate (spendgate), which reserves and tracks holds in Redis with per-window
-- counters — there is no durable Postgres reservation to keep. The feature had
-- zero production consumers, so the table, its sqlc queries, the Go service +
-- handlers + routes, and the SDK methods are removed outright (pre-launch: no
-- data migration). Held balance is now Redis-only (#505); deriveBalance reports
-- HeldBalance: 0.
-- =============================================================================

DROP TABLE IF EXISTS openrails.money_windows;
