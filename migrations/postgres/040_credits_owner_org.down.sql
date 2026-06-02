-- Reverse of 040: remove the owner-org ownership of the credit tables.
-- NOTE: migratekit (LoadFromFS) only applies *.up.sql files; this .down.sql is
-- kept for documentation / manual rollback and is NOT auto-loaded.
--
-- Drops the owner-scoped indexes/uniques and the owner_id column on the three
-- credit tables. Safe to run only if no code path depends on owner_id (i.e.
-- before/without the org-owned credit writers being enabled). user_id (actor
-- attribution) and the original user-scoped uniques are untouched, so existing
-- balances are unaffected.

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

DROP INDEX IF EXISTS billing.idx_user_credit_balances_owner;
DROP INDEX IF EXISTS billing.uq_user_credit_balances_owner_type;
DROP INDEX IF EXISTS billing.idx_credit_transactions_owner;
DROP INDEX IF EXISTS billing.uniq_credit_hold_idem_owner;
DROP INDEX IF EXISTS billing.uniq_credit_deposit_idem_owner;
DROP INDEX IF EXISTS billing.uniq_credit_withdrawal_idem_owner;
DROP INDEX IF EXISTS billing.idx_credit_blocks_owner;

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
