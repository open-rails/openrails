-- =============================================================================
-- 071 — Hard-cut billing identity vocabulary to tenant_subject_id / invoker_id
--
-- OpenRails billing rows now use:
--   tenant_id     = host/application namespace
--   tenant_subject_id  = AuthKit tenant/personal tenant-subject whose balance/account is charged
--   invoker_id    = principal that invoked the billable operation
--
-- This physically renames the credit, usage, invoice, admission, blocklist, and
-- budget columns created by earlier migrations. There are no legacy aliases.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

ALTER TABLE billing.user_credit_balances
    RENAME COLUMN owner_id TO tenant_subject_id;
ALTER TABLE billing.user_credit_balances
    RENAME COLUMN user_id TO invoker_id;

ALTER TABLE billing.credit_transactions
    RENAME COLUMN owner_id TO tenant_subject_id;
ALTER TABLE billing.credit_transactions
    RENAME COLUMN user_id TO invoker_id;

ALTER TABLE billing.credit_blocks
    RENAME COLUMN owner_id TO tenant_subject_id;
ALTER TABLE billing.credit_blocks
    RENAME COLUMN user_id TO invoker_id;

ALTER TABLE billing.credit_account_settings
    RENAME COLUMN owner_id TO tenant_subject_id;

ALTER TABLE billing.credit_spend_limits
    RENAME COLUMN owner_id TO tenant_subject_id;
ALTER TABLE billing.credit_spend_limits
    RENAME COLUMN invoker TO invoker_id;

ALTER TABLE billing.usage_events
    RENAME COLUMN owner_id TO tenant_subject_id;
ALTER TABLE billing.usage_events
    RENAME COLUMN user_id TO invoker_id;

ALTER TABLE billing.invoices
    RENAME COLUMN owner_id TO tenant_subject_id;

ALTER TABLE billing.tier_policies
    RENAME COLUMN owner_id TO tenant_subject_id;

ALTER TABLE billing.payment_blocklist
    RENAME COLUMN owner_id TO tenant_subject_id;

ALTER TABLE billing.budget_reservations
    RENAME COLUMN owner_id TO tenant_subject_id;
ALTER TABLE billing.budget_reservations
    RENAME COLUMN actor TO invoker_id;

ALTER INDEX IF EXISTS billing.uq_user_credit_balances_owner_type RENAME TO uq_user_credit_balances_payer_type;
ALTER INDEX IF EXISTS billing.uniq_credit_hold_idem_owner RENAME TO uniq_credit_hold_idem_payer;
ALTER INDEX IF EXISTS billing.uniq_credit_deposit_idem_owner RENAME TO uniq_credit_deposit_idem_payer;
ALTER INDEX IF EXISTS billing.uniq_credit_withdrawal_idem_owner RENAME TO uniq_credit_withdrawal_idem_payer;
ALTER INDEX IF EXISTS billing.idx_credit_transactions_owner RENAME TO idx_credit_transactions_payer;
ALTER INDEX IF EXISTS billing.idx_credit_blocks_owner RENAME TO idx_credit_blocks_payer;
ALTER INDEX IF EXISTS billing.idx_credit_transactions_owner_actor RENAME TO idx_credit_transactions_payer_invoker;
ALTER INDEX IF EXISTS billing.idx_user_credit_balances_tenant_user RENAME TO idx_user_credit_balances_tenant_invoker;
ALTER INDEX IF EXISTS billing.idx_credit_transactions_tenant_user RENAME TO idx_credit_transactions_tenant_invoker;
ALTER INDEX IF EXISTS billing.idx_credit_blocks_tenant_user RENAME TO idx_credit_blocks_tenant_invoker;
ALTER INDEX IF EXISTS billing.uq_credit_account_settings_owner_type RENAME TO uq_credit_account_settings_payer_type;
ALTER INDEX IF EXISTS billing.uq_credit_spend_limits_owner_invoker RENAME TO uq_credit_spend_limits_payer_invoker;
ALTER INDEX IF EXISTS billing.ix_usage_events_owner_time RENAME TO ix_usage_events_payer_time;
ALTER INDEX IF EXISTS billing.ix_usage_events_owner_type_time RENAME TO ix_usage_events_payer_type_time;
ALTER INDEX IF EXISTS billing.ix_invoices_owner RENAME TO ix_invoices_payer;

COMMENT ON COLUMN billing.user_credit_balances.tenant_subject_id IS
    'AuthKit tenant/personal tenant-subject whose balance is charged.';
COMMENT ON COLUMN billing.user_credit_balances.invoker_id IS
    'Principal that caused the balance row to be created or updated.';
COMMENT ON COLUMN billing.credit_transactions.tenant_subject_id IS
    'AuthKit tenant/personal tenant-subject charged by this ledger transaction.';
COMMENT ON COLUMN billing.credit_transactions.invoker_id IS
    'Principal that invoked the billable operation.';
COMMENT ON COLUMN billing.credit_blocks.tenant_subject_id IS
    'AuthKit tenant/personal tenant-subject that owns this credit block.';
COMMENT ON COLUMN billing.credit_blocks.invoker_id IS
    'Principal that caused this credit block to be created.';
COMMENT ON COLUMN billing.usage_events.tenant_subject_id IS
    'AuthKit tenant/personal tenant-subject billed for this usage event.';
COMMENT ON COLUMN billing.usage_events.invoker_id IS
    'Principal that invoked this metered usage event.';
COMMENT ON COLUMN billing.budget_reservations.tenant_subject_id IS
    'AuthKit tenant/personal tenant-subject whose budget is reserved.';
COMMENT ON COLUMN billing.budget_reservations.invoker_id IS
    'Principal whose rolling money-budget windows are capped.';
COMMENT ON COLUMN billing.credit_spend_limits.tenant_subject_id IS
    'AuthKit tenant/personal tenant-subject whose per-invoker spend limit applies.';
COMMENT ON COLUMN billing.credit_spend_limits.invoker_id IS
    'Principal whose spend is capped by this row.';
