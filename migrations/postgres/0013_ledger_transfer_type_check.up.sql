-- #832: ledger_accounts.account_type has a closed-set CHECK; transfer_type was
-- free text. idx_ledger_transfers_lot_once — the index that stops a credit lot
-- being deposited, expired or revoked twice — is PARTIAL on three named
-- transfer_type literals, so a typo in a new type silently fell outside it and
-- the duplicate posted. Close the vocabulary. Kept in lockstep with the Go
-- constants by TestTransferTypeVocabularyMatchesSchema.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- Already applied. NOT VALID buys nothing inside a single-transaction migrator
-- — the ADD's ACCESS EXCLUSIVE lock is held to COMMIT either way — and it would
-- leave the file claiming an unvalidated constraint that every migrated
-- database has already validated. A NEW closed-set CHECK on a hot table should
-- still be ADD ... NOT VALID here plus VALIDATE CONSTRAINT in a later file.
ALTER TABLE openrails.ledger_transfers
    -- squawk-ignore constraint-missing-not-valid
    ADD CONSTRAINT ledger_transfers_type_check CHECK ((transfer_type = ANY (ARRAY[
        'deposit'::text,
        'credit_spend'::text,
        'credit_expire'::text,
        'credit_revoke'::text,
        'credit_reinstate'::text,
        'owed_accrual'::text,
        'owed_payment'::text
    ])));
