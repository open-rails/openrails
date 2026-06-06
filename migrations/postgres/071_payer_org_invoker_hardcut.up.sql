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

DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT *
        FROM (VALUES
            ('user_credit_balances', 'owner_id', 'tenant_subject_id'),
            ('user_credit_balances', 'user_id', 'invoker_id'),
            ('credit_transactions', 'owner_id', 'tenant_subject_id'),
            ('credit_transactions', 'user_id', 'invoker_id'),
            ('credit_blocks', 'owner_id', 'tenant_subject_id'),
            ('credit_blocks', 'user_id', 'invoker_id'),
            ('credit_account_settings', 'owner_id', 'tenant_subject_id'),
            ('credit_spend_limits', 'owner_id', 'tenant_subject_id'),
            ('credit_spend_limits', 'invoker', 'invoker_id'),
            ('usage_events', 'owner_id', 'tenant_subject_id'),
            ('usage_events', 'user_id', 'invoker_id'),
            ('invoices', 'owner_id', 'tenant_subject_id'),
            ('tier_policies', 'owner_id', 'tenant_subject_id'),
            ('payment_blocklist', 'owner_id', 'tenant_subject_id'),
            ('budget_reservations', 'owner_id', 'tenant_subject_id'),
            ('budget_reservations', 'actor', 'invoker_id')
        ) AS v(table_name, old_column, new_column)
    LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = 'billing'
              AND table_name = r.table_name
              AND column_name = r.old_column
        ) AND NOT EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_schema = 'billing'
              AND table_name = r.table_name
              AND column_name = r.new_column
        ) THEN
            EXECUTE format('ALTER TABLE billing.%I RENAME COLUMN %I TO %I', r.table_name, r.old_column, r.new_column);
        END IF;
    END LOOP;
END $$;

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
