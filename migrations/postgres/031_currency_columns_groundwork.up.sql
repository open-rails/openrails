-- #494 groundwork (owner 2026-06-15): introduce the `currency` columns on the
-- invoker-bearing tables that lack them, so the money/budget model stops implicitly
-- assuming USD. ADDITIVE + low-risk: default 'USD' (the current implicit microdollar =
-- USD micros) so existing rows + existing inserts are unaffected. The `*_micros`
-- columns STAY — "micros" is the currency-NEUTRAL sub-cent unit (millionths of THIS
-- row's currency, à la Google Ads; NOT microdollars), now disambiguated by `currency`.
--
-- NOT in this migration (deferred to the rest of #494, coupled with the FX converter):
--   * per-currency KEYING of the budget engine (adding currency to the budget UNIQUE
--     keys + threading a currency through admit->budget, which has no currency concept
--     today) — meaningful only with the converter for cross-currency deduction.
--   * the budget_reservations -> budget_inflight_holds rename (cosmetic; bundle with
--     the keying migration to avoid a standalone model/query/Go churn pass).
--   * native-currency validation (USD/EUR/JPY allowlist) — add with the converter.
-- money_transactions + money_spend_limits ALREADY carry `currency` (no change here).

SET lock_timeout = '10s';
SET statement_timeout = '300s';

ALTER TABLE openrails.usage_events        ADD COLUMN IF NOT EXISTS currency text NOT NULL DEFAULT 'USD';
ALTER TABLE openrails.budget_window_state ADD COLUMN IF NOT EXISTS currency text NOT NULL DEFAULT 'USD';
ALTER TABLE openrails.budget_reservations ADD COLUMN IF NOT EXISTS currency text NOT NULL DEFAULT 'USD';
