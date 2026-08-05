SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';
ALTER TABLE openrails.ledger_transfers DROP CONSTRAINT ledger_transfers_type_check;
ALTER TABLE openrails.ledger_transfers
    ADD CONSTRAINT ledger_transfers_type_check CHECK ((transfer_type = ANY (ARRAY[
        'deposit'::text, 'credit_spend'::text, 'credit_expire'::text, 'credit_revoke'::text,
        'credit_reinstate'::text, 'owed_accrual'::text, 'owed_payment'::text
    ]))) NOT VALID;
