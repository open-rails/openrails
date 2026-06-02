-- Down: drop the spend-policy + per-invoker spend-limit tables (issue #237).
DROP INDEX IF EXISTS billing.idx_credit_transactions_owner_actor;
DROP TABLE IF EXISTS billing.credit_spend_limits;
DROP TABLE IF EXISTS billing.credit_account_settings;
