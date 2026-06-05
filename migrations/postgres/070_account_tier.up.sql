-- 070 — graduated tier on the credit account (issue #298 graduation).
-- The tier a customer has EARNED from cumulative paid spend. Admission uses it
-- when the caller doesn't supply an explicit tier. RLS already on the table.
SET lock_timeout      = '10s';
SET statement_timeout = '300s';

ALTER TABLE billing.credit_account_settings ADD COLUMN IF NOT EXISTS tier TEXT;
