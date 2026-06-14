--
-- Per-tenant arrears credit-line: admin-set credit_limit_micros (#489).
--
-- The existing arrears mode already supports a credit line via
-- max_outstanding_owed_micros, but that ceiling is (a) enforced on the
-- SETTLEMENT path (SpendCredits/AccrueOwed) and (b) SELF-SERVE (set via
-- UpsertAccountSettings). #489 adds an EXPLICIT, OPERATOR/ADMIN-ONLY credit limit
-- enforced at ADMIT/HOLD time: under billing_mode=arrears the balance may go
-- NEGATIVE up to credit_limit_micros, and AdmitHold denies with
-- "insufficient_credit" when a new hold would exceed it.
--
-- DEFAULT 0 = OFF: credit_limit_micros=0 leaves both prepaid accounts AND
-- existing arrears accounts behaving exactly as before (no admit-time negative
-- balance). Only an operator setting credit_limit_micros > 0 opens the credit
-- line. NOT self-serve — written via the dedicated SetMoneyAccountCreditLimit
-- query, never UpsertMoneyAccountSettings.
--

ALTER TABLE openrails.money_accounts
    ADD COLUMN IF NOT EXISTS credit_limit_micros bigint NOT NULL DEFAULT 0;

ALTER TABLE openrails.money_accounts
    DROP CONSTRAINT IF EXISTS money_accounts_credit_limit_nonneg_chk;
ALTER TABLE openrails.money_accounts
    ADD CONSTRAINT money_accounts_credit_limit_nonneg_chk CHECK ((credit_limit_micros >= 0));

COMMENT ON COLUMN openrails.money_accounts.credit_limit_micros IS 'Admin-set per-account arrears credit line (#489): under billing_mode=arrears the balance may go negative up to this amount; AdmitHold denies insufficient_credit when a new hold would exceed it. 0 = off (prepaid/existing-arrears behavior unchanged). NOT self-serve.';
