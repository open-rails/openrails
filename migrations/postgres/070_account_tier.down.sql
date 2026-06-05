SET lock_timeout = '10s';
ALTER TABLE billing.credit_account_settings DROP COLUMN IF EXISTS tier;
