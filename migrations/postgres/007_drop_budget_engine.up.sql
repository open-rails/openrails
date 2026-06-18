-- =============================================================================
-- #513 HARD CUT — drop the Postgres rolling-budget engine tables.
--
-- Budget accounting (reserved/used per fixed window) moved to the Redis spendgate
-- (internal/modules/admission/spendgate): admit/capture/release are one atomic Lua
-- EVAL, with per-user-staggered windows derived deterministically from the
-- customer-scoped key. The locked-tx tables — budget_inflight_holds (the
-- per-request reservation rows) and budget_window_state (the per-window anchor row
-- taken FOR UPDATE) — are no longer read or written by any code path.
--
-- openrails.scoped_spend_caps is KEPT: it stores the {scope, owner, windows[]} cap
-- CONFIG the admission policy loader reads into the spendgate policy.
-- =============================================================================

DROP TABLE IF EXISTS openrails.budget_inflight_holds;
DROP TABLE IF EXISTS openrails.budget_window_state;
