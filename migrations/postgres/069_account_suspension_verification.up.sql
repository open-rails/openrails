-- =============================================================================
-- 069 — Account suspension + payment-method-verification state (issue #299)
--
-- Adds suspension + payment-method-verification columns to the per-(tenant,
-- tenant subject, credit_type) account settings row. These record state only:
--   - verified_payment_method / verified_at: set true after a successful $1
--     auth-and-void verification charge (the charge itself is a separate slice).
--   - suspended_at / suspend_reason: set when an account is suspended.
--
-- The table already has RLS (migration 050 tenant_isolation); new columns
-- inherit it. Wiring suspension into the admission deny path is a separate slice.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

ALTER TABLE billing.credit_account_settings
    ADD COLUMN IF NOT EXISTS verified_payment_method BOOLEAN     NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS verified_at             TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS suspended_at            TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS suspend_reason          TEXT;

COMMENT ON COLUMN billing.credit_account_settings.verified_payment_method IS
    'True once the account has a verified payment method (set after a successful $1 auth-and-void verification charge — issue #299). The charge itself is a separate slice.';
COMMENT ON COLUMN billing.credit_account_settings.suspended_at IS
    'When set, the account is suspended (issue #299). Admission-deny-on-suspended wiring is a separate slice.';
