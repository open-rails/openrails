-- Reverse of 040: remove the tenant-subject ownership of the credit tables.
-- NOTE: migratekit (LoadFromFS) only applies *.up.sql files; this .down.sql is
-- kept for documentation / manual rollback and is NOT auto-loaded.
--
-- Restores the legacy user-scoped uniques, drops the owner+tenant-scoped
-- indexes/uniques and the owner_id column on the three credit tables. user_id
-- (actor attribution) is untouched.

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP INDEX IF EXISTS billing.uq_user_credit_balances_owner_type;
DROP INDEX IF EXISTS billing.idx_credit_transactions_owner;
DROP INDEX IF EXISTS billing.uniq_credit_hold_idem_owner;
DROP INDEX IF EXISTS billing.uniq_credit_deposit_idem_owner;
DROP INDEX IF EXISTS billing.uniq_credit_withdrawal_idem_owner;
DROP INDEX IF EXISTS billing.idx_credit_blocks_owner;

-- Restore the legacy user-scoped uniques.
ALTER TABLE billing.user_credit_balances
    ADD CONSTRAINT user_credit_balances_user_id_credit_type_id_key UNIQUE (user_id, credit_type_id);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_hold_idem
    ON billing.credit_transactions(user_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'hold';
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_deposit_idem
    ON billing.credit_transactions(user_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'deposit' AND source_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_withdrawal_idem
    ON billing.credit_transactions(user_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'withdrawal' AND source_id IS NOT NULL;

DO $$
DECLARE
    t TEXT;
    credit_tables CONSTANT TEXT[] := ARRAY[
        'user_credit_balances',
        'credit_transactions',
        'credit_blocks'
    ];
BEGIN
    FOREACH t IN ARRAY credit_tables LOOP
        IF EXISTS (
            SELECT 1 FROM information_schema.tables
            WHERE table_schema = 'billing' AND table_name = t
        ) THEN
            EXECUTE format('ALTER TABLE billing.%I DROP COLUMN IF EXISTS owner_id', t);
        END IF;
    END LOOP;
END $$;
