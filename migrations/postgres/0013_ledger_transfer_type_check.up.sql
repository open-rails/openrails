-- #832: ledger_accounts.account_type has a closed-set CHECK; transfer_type was
-- free text. idx_ledger_transfers_lot_once — the index that stops a credit lot
-- being deposited, expired or revoked twice — is PARTIAL on three named
-- transfer_type literals, so a typo in a new type silently fell outside it and
-- the duplicate posted. Close the vocabulary. Kept in lockstep with the Go
-- constants by TestTransferTypeVocabularyMatchesSchema.

ALTER TABLE openrails.ledger_transfers
    ADD CONSTRAINT ledger_transfers_type_check CHECK ((transfer_type = ANY (ARRAY[
        'deposit'::text,
        'credit_spend'::text,
        'credit_expire'::text,
        'credit_revoke'::text,
        'credit_reinstate'::text,
        'owed_accrual'::text,
        'owed_payment'::text
    ])));
