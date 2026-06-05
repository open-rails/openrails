-- Down migration for 069 — drop suspension + payment-method-verification columns (issue #299).
SET lock_timeout      = '10s';
SET statement_timeout = '300s';

ALTER TABLE billing.credit_account_settings
    DROP COLUMN IF EXISTS suspend_reason,
    DROP COLUMN IF EXISTS suspended_at,
    DROP COLUMN IF EXISTS verified_at,
    DROP COLUMN IF EXISTS verified_payment_method;
