-- Down migration for 071 — restore pre-hard-cut owner/user/actor column names.

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

ALTER INDEX IF EXISTS billing.uq_user_credit_balances_payer_type RENAME TO uq_user_credit_balances_owner_type;
ALTER INDEX IF EXISTS billing.uniq_credit_hold_idem_payer RENAME TO uniq_credit_hold_idem_owner;
ALTER INDEX IF EXISTS billing.uniq_credit_deposit_idem_payer RENAME TO uniq_credit_deposit_idem_owner;
ALTER INDEX IF EXISTS billing.uniq_credit_withdrawal_idem_payer RENAME TO uniq_credit_withdrawal_idem_owner;
ALTER INDEX IF EXISTS billing.idx_credit_transactions_payer RENAME TO idx_credit_transactions_owner;
ALTER INDEX IF EXISTS billing.idx_credit_blocks_payer RENAME TO idx_credit_blocks_owner;
ALTER INDEX IF EXISTS billing.idx_credit_transactions_payer_invoker RENAME TO idx_credit_transactions_owner_actor;
ALTER INDEX IF EXISTS billing.idx_user_credit_balances_tenant_invoker RENAME TO idx_user_credit_balances_tenant_user;
ALTER INDEX IF EXISTS billing.idx_credit_transactions_tenant_invoker RENAME TO idx_credit_transactions_tenant_user;
ALTER INDEX IF EXISTS billing.idx_credit_blocks_tenant_invoker RENAME TO idx_credit_blocks_tenant_user;
ALTER INDEX IF EXISTS billing.uq_credit_account_settings_payer_type RENAME TO uq_credit_account_settings_owner_type;
ALTER INDEX IF EXISTS billing.uq_credit_spend_limits_payer_invoker RENAME TO uq_credit_spend_limits_owner_invoker;
ALTER INDEX IF EXISTS billing.ix_usage_events_payer_time RENAME TO ix_usage_events_owner_time;
ALTER INDEX IF EXISTS billing.ix_usage_events_payer_type_time RENAME TO ix_usage_events_owner_type_time;
ALTER INDEX IF EXISTS billing.ix_invoices_payer RENAME TO ix_invoices_owner;

ALTER TABLE billing.budget_reservations
    RENAME COLUMN invoker_id TO actor;
ALTER TABLE billing.budget_reservations
    RENAME COLUMN payer_org_id TO owner_id;

ALTER TABLE billing.payment_blocklist
    RENAME COLUMN payer_org_id TO owner_id;

ALTER TABLE billing.tier_policies
    RENAME COLUMN payer_org_id TO owner_id;

ALTER TABLE billing.invoices
    RENAME COLUMN payer_org_id TO owner_id;

ALTER TABLE billing.usage_events
    RENAME COLUMN invoker_id TO user_id;
ALTER TABLE billing.usage_events
    RENAME COLUMN payer_org_id TO owner_id;

ALTER TABLE billing.credit_spend_limits
    RENAME COLUMN invoker_id TO invoker;
ALTER TABLE billing.credit_spend_limits
    RENAME COLUMN payer_org_id TO owner_id;

ALTER TABLE billing.credit_account_settings
    RENAME COLUMN payer_org_id TO owner_id;

ALTER TABLE billing.credit_blocks
    RENAME COLUMN invoker_id TO user_id;
ALTER TABLE billing.credit_blocks
    RENAME COLUMN payer_org_id TO owner_id;

ALTER TABLE billing.credit_transactions
    RENAME COLUMN invoker_id TO user_id;
ALTER TABLE billing.credit_transactions
    RENAME COLUMN payer_org_id TO owner_id;

ALTER TABLE billing.user_credit_balances
    RENAME COLUMN invoker_id TO user_id;
ALTER TABLE billing.user_credit_balances
    RENAME COLUMN payer_org_id TO owner_id;
