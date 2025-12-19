ALTER TABLE billing.products
    DROP COLUMN IF EXISTS credits_spec;

DROP TABLE IF EXISTS billing.credit_holds;
DROP TABLE IF EXISTS billing.credit_expiry_batches;
DROP TABLE IF EXISTS billing.credit_transactions;
DROP TABLE IF EXISTS billing.user_credit_balances;
DROP TABLE IF EXISTS billing.credit_types;
