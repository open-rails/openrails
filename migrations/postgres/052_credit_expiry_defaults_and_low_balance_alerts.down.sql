-- =============================================================================
-- 052 (down) — drop default-expiry + low-balance alert schema (issue #240)
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP INDEX IF EXISTS billing.idx_user_credit_balances_low_balance_alert;

ALTER TABLE billing.user_credit_balances
    DROP COLUMN IF EXISTS last_low_balance_alert_at;

ALTER TABLE billing.credit_types
    DROP COLUMN IF EXISTS low_balance_threshold;

ALTER TABLE billing.credit_types
    DROP COLUMN IF EXISTS default_credit_expiry_days;
