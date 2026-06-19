-- =============================================================================
-- #512/#513 — retire the dormant in-ledger two-phase (pending) apparatus.
--
-- The #512 ledger was a faithful TigerBeetle port including two-phase transfers
-- (pending -> post_pending|void_pending) for authorize/capture/release. But #513
-- moved the admission HOLD into Redis (the spendgate); the durable ledger only
-- ever writes `posted` transfers. So the pending machinery had ZERO live callers
-- (ledger.Authorize/Capture/Release were test-only), the *_pending counters were
-- always 0, and — with HoldExpiryWorker deleted this arc — there was no expiry to
-- reclaim a stale pending anyway. It was correct-but-incomplete dead weight.
--
-- This collapses the ledger to a single-phase, posted-only double-entry ledger
-- (still fully TigerBeetle-correct for the posted model): every transfer posts to
-- balance immediately, conservation (sum of account balances = 0) is unchanged.
-- Holds remain Redis-only (#505); GetAdmissionCapacity reports held = 0.
-- =============================================================================

-- 1) Replace the counter trigger with a posted-only version (no pending branch,
--    no *_pending counters). Every transfer now posts to the balance counters.
CREATE OR REPLACE FUNCTION openrails.ledger_transfers_apply_counters() RETURNS trigger
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = openrails, pg_catalog
    AS $$
DECLARE
    acc openrails.ledger_accounts%ROWTYPE;
    debit openrails.ledger_accounts%ROWTYPE;
    credit openrails.ledger_accounts%ROWTYPE;
    debit_balance bigint;
    credit_balance bigint;
BEGIN
    FOR acc IN
        SELECT *
        FROM openrails.ledger_accounts
        WHERE merchant_id = NEW.merchant_id
          AND id IN (NEW.debit_account_id, NEW.credit_account_id)
        ORDER BY id
        FOR UPDATE
    LOOP
        IF acc.id = NEW.debit_account_id THEN
            debit := acc;
        ELSIF acc.id = NEW.credit_account_id THEN
            credit := acc;
        END IF;
    END LOOP;

    IF debit.id IS NULL OR credit.id IS NULL THEN
        RAISE EXCEPTION 'ledger_transfers: debit/credit account not found';
    END IF;

    IF debit.currency <> NEW.currency OR credit.currency <> NEW.currency THEN
        RAISE EXCEPTION 'ledger_transfers: cross-currency transfer (debit=%, credit=%, transfer=%) - a transfer never crosses ledgers', debit.currency, credit.currency, NEW.currency;
    END IF;

    debit_balance := debit.credits_posted - debit.debits_posted - NEW.amount;
    credit_balance := credit.debits_posted - credit.credits_posted - NEW.amount;
    IF debit.debits_must_not_exceed_credits AND debit_balance < -NEW.allow_debit_negative_up_to THEN
        RAISE EXCEPTION 'ledger_insufficient_funds: balance %, amount %, floor %', debit.credits_posted - debit.debits_posted, NEW.amount, NEW.allow_debit_negative_up_to;
    END IF;
    IF credit.credits_must_not_exceed_debits AND credit_balance < 0 THEN
        RAISE EXCEPTION 'ledger_credit_constraint: credit account % would exceed debits', NEW.credit_account_id;
    END IF;

    UPDATE openrails.ledger_accounts
    SET debits_posted = debits_posted + NEW.amount
    WHERE id = NEW.debit_account_id;
    UPDATE openrails.ledger_accounts
    SET credits_posted = credits_posted + NEW.amount
    WHERE id = NEW.credit_account_id;

    RETURN NEW;
END;
$$;

-- 2) Drop the pending constraints + the pending-resolution unique index.
ALTER TABLE openrails.ledger_transfers
    DROP CONSTRAINT IF EXISTS ledger_transfers_phase_check,
    DROP CONSTRAINT IF EXISTS ledger_transfers_pending_link,
    DROP CONSTRAINT IF EXISTS ledger_transfers_pending_fk;
DROP INDEX IF EXISTS openrails.uq_ledger_transfers_pending_resolution;

-- 3) Drop the now-unreferenced pending columns.
ALTER TABLE openrails.ledger_transfers
    DROP COLUMN IF EXISTS phase,
    DROP COLUMN IF EXISTS pending_id;
ALTER TABLE openrails.ledger_accounts
    DROP COLUMN IF EXISTS credits_pending,
    DROP COLUMN IF EXISTS debits_pending;
