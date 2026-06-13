-- =============================================================================
-- Built-in credit type: micro-dollars (1 unit = 1e-6 USD).
--
-- The credit ledger is denominated in micro-dollars; this row is an application
-- invariant, not something services create at runtime. Callers reference it by
-- name ("usd_micro").
-- =============================================================================

SET lock_timeout = '10s';
SET statement_timeout = '300s';

INSERT INTO openrails.credit_types (name, display_name, unit, decimal_places, is_active)
VALUES ('usd_micro', 'US dollars (micro)', 'usd', 6, true)
ON CONFLICT (name) DO NOTHING;
