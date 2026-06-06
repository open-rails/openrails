-- =============================================================================
-- 040 — Tenant-subject-owned API usage credits (issue #221)
--
-- Makes the credit tables OWNER-ORG-owned (the payer / billing owner) while
-- keeping user_id for ACTOR attribution (who caused usage).
--
-- THE THREE-WAY IDENTITY SPLIT (see pkg/identity):
--   * operator tenant  — admin authority (#224), NOT stored on credit rows;
--   * owner_id       — the org that OWNS the balance / is billed (this migration);
--   * user_id        — the ACTOR that caused usage (kept, unchanged).
--
-- HARDCUT / GREENFIELD: this branch replaces the pre-refactor user-scoped credit
-- schema wholesale. There are NO pre-existing rows to preserve, so:
--   * owner_id is created NOT NULL from the start (writers supply the real owner
--     AuthKit tenant id at write time; there is NO stand-in/deterministic backfill
--     and NO later reconciliation to a "real" personal org — the value written
--     IS the real tenant subject id);
--   * the legacy user-scoped idempotency / balance uniques are REPLACED with
--     owner+tenant-scoped ones (old dropped, new created — never both);
--   * user_id is kept for ACTOR attribution (NOT dropped, NOT renamed); the Go
--     model exposes it as ActorUserID.
--
-- tenant_id was added NOT NULL (defaulted to the 'default' tenant) by migration
-- 039 for these tables, so it is already present and enforced here.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- -----------------------------------------------------------------------------
-- Add a NOT NULL owner_id (tenant subject) column to the three credit tables.
-- Greenfield: no existing rows, so the column is created NOT NULL directly with
-- no backfill. Writers stamp the real tenant subject id.
-- -----------------------------------------------------------------------------

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
            EXECUTE format(
                'ALTER TABLE billing.%I ADD COLUMN IF NOT EXISTS owner_id UUID NOT NULL',
                t
            );
            EXECUTE format(
                $cmt$COMMENT ON COLUMN billing.%I.owner_id IS 'Tenant subject that OWNS this balance / is billed (issue #221, payer/billing owner). NOT NULL; the real AuthKit tenant subject id supplied by the writer. user_id is kept for ACTOR attribution.'$cmt$,
                t
            );
        END IF;
    END LOOP;
END $$;

-- -----------------------------------------------------------------------------
-- Replace the legacy user-scoped uniques with owner+tenant-scoped ones. HARDCUT:
-- the old user-scoped uniques are DROPPED and the owner+tenant-scoped ones are
-- CREATED — the two never coexist. Idempotency is now unique per
-- (tenant, owner, credit_type, source, source_id) per transaction kind, and the
-- balance row is unique per (tenant, owner, credit_type).
-- -----------------------------------------------------------------------------

-- Balance: UNIQUE(user_id, credit_type_id) -> (tenant, owner, credit_type).
ALTER TABLE billing.user_credit_balances
    DROP CONSTRAINT IF EXISTS user_credit_balances_user_id_credit_type_id_key;
-- This unique index also serves the GetBalance / lockBalance hot-path lookup
-- on (tenant_id, owner_id, credit_type_id).
CREATE UNIQUE INDEX IF NOT EXISTS uq_user_credit_balances_owner_type
    ON billing.user_credit_balances (tenant_id, owner_id, credit_type_id);

-- Idempotency uniques per transaction kind: user-scoped -> owner+tenant-scoped.
DROP INDEX IF EXISTS billing.uniq_credit_hold_idem;
DROP INDEX IF EXISTS billing.uniq_credit_deposit_idem;
DROP INDEX IF EXISTS billing.uniq_credit_withdrawal_idem;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_hold_idem_owner
    ON billing.credit_transactions (tenant_id, owner_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'hold';
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_deposit_idem_owner
    ON billing.credit_transactions (tenant_id, owner_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'deposit' AND source_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_withdrawal_idem_owner
    ON billing.credit_transactions (tenant_id, owner_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'withdrawal' AND source_id IS NOT NULL;

-- Owner-scoped transaction history.
CREATE INDEX IF NOT EXISTS idx_credit_transactions_owner
    ON billing.credit_transactions (tenant_id, owner_id, credit_type_id, created_at DESC);

-- Owner-scoped FIFO block consumption.
CREATE INDEX IF NOT EXISTS idx_credit_blocks_owner
    ON billing.credit_blocks (tenant_id, owner_id, credit_type_id, expires_at ASC);
