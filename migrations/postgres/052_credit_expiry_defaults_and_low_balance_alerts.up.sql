-- =============================================================================
-- 052 — Default purchased-credit expiry + low-balance alert state (issue #240)
--
-- Completes two pieces the original credits system (#116) designed but never
-- shipped:
--
--   (1) DEFAULT PURCHASED-CREDIT EXPIRY. Deposits of purchased credits should get
--       a sensible default expiry instead of living forever. We add a per-credit
--       type `default_credit_expiry_days`. When a Deposit is made with no explicit
--       ExpiresAt, the service stamps now + default_credit_expiry_days. NULL means
--       "no default" (the credit type is non-expiring unless the caller passes an
--       explicit expiry) — this preserves the pre-#240 behavior for any type that
--       wants it. A global fallback default (365 days) lives in config so an
--       operator can set a floor without touching every credit type.
--
--   (2) LOW-BALANCE ALERTS — the deferred #116 Phase 4c/8b. The alert job needs a
--       threshold to compare against and a per-owner "last alerted" timestamp so
--       it can respect a cooldown and avoid re-alerting every run. We add:
--         * billing.credit_types.low_balance_threshold       (BIGINT, NULL=off)
--         * billing.user_credit_balances.last_low_balance_alert_at (TIMESTAMPTZ)
--       The threshold is per credit TYPE for now. The alert state is per
--       (tenant, owner, credit_type) balance row — i.e. owner/tenant-scoped
--       (issue #221/#223), so each owner is alerted independently.
--
-- #237 BOUNDARY: issue #237 introduces a per-owner billing-account-settings model
-- that will eventually OWN per-owner expiry defaults + per-owner low-balance
-- thresholds (so a specific team org can override the type-level threshold). That
-- table is being built in a SEPARATE branch and is NOT available here, so #240
-- intentionally puts the defaults/threshold on the credit_type (plus config for
-- the global expiry fallback). When #237 lands, its per-owner settings can take
-- precedence over these type-level values; the alert state column added here
-- (last_low_balance_alert_at) is unaffected and remains the per-owner cooldown
-- anchor.
--
-- HARDCUT / additive: all columns are nullable additions with safe defaults; no
-- backfill is required. The new columns inherit the migration-039/050 tenant
-- scoping and RLS already in force on these tables.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- -----------------------------------------------------------------------------
-- (1) Default purchased-credit expiry (per credit type). NULL = no default.
-- -----------------------------------------------------------------------------
ALTER TABLE billing.credit_types
    ADD COLUMN IF NOT EXISTS default_credit_expiry_days INT;

COMMENT ON COLUMN billing.credit_types.default_credit_expiry_days IS
    'Default expiry (in days) applied to deposits of this credit type when the caller does not pass an explicit ExpiresAt (issue #240). NULL = no per-type default; the service may then fall back to the global config default. #237 per-owner settings may later override this.';

-- -----------------------------------------------------------------------------
-- (2a) Low-balance alert threshold (per credit type). NULL = alerting disabled.
--      Compared against AVAILABLE balance (balance - held_balance).
-- -----------------------------------------------------------------------------
ALTER TABLE billing.credit_types
    ADD COLUMN IF NOT EXISTS low_balance_threshold BIGINT;

COMMENT ON COLUMN billing.credit_types.low_balance_threshold IS
    'Low-balance alert threshold for this credit type (issue #240, completing the deferred #116 alert job). When an owner''s AVAILABLE balance (balance - held_balance) drops below this, the low-balance alert job emits a notification. NULL = alerting disabled for this type. #237 per-owner settings may later override this per owner.';

-- -----------------------------------------------------------------------------
-- (2b) Per-owner low-balance alert state. last_low_balance_alert_at lets the
--      alert job enforce a cooldown so a persistently-low owner is alerted at
--      most once per cooldown window rather than every run. Keyed implicitly by
--      the balance row's (tenant_id, owner_id, credit_type_id).
-- -----------------------------------------------------------------------------
ALTER TABLE billing.user_credit_balances
    ADD COLUMN IF NOT EXISTS last_low_balance_alert_at TIMESTAMPTZ;

COMMENT ON COLUMN billing.user_credit_balances.last_low_balance_alert_at IS
    'Timestamp of the most recent low-balance alert emitted for this owner+credit_type (issue #240). NULL = never alerted. The alert job only re-alerts when this is NULL or older than the configured cooldown, then updates it. Owner/tenant-scoped (issue #221/#223).';

-- Partial index to make the alert job''s scan (rows that are candidates for an
-- alert: a threshold-eligible, never-or-stale-alerted balance) cheap. The job
-- still filters on the per-type threshold join, but this narrows the balance
-- side to rows worth considering.
CREATE INDEX IF NOT EXISTS idx_user_credit_balances_low_balance_alert
    ON billing.user_credit_balances (tenant_id, credit_type_id, last_low_balance_alert_at);
