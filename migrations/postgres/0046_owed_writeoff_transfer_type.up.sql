-- or#897: voiding an invoice must move the LEDGER, not just the invoice row.
--
-- Surfaced by making the ledger the only exposure substrate. VoidInvoice zeroed
-- invoices.amount_due and stopped there, which was invisible while exposure was
-- invoice-derived. Once exposure is the arrears account, a voided invoice left
-- the accrual standing: the debt was cancelled on the invoice and permanent on
-- the ledger, so the payer stayed capped forever for a bill nobody owes. That
-- is the mirror of the ledger-less-invoice-debt bug, and it gets fixed in the
-- path that causes it.
--
-- owed_writeoff is the exact inverse of owed_accrual (DR platform_revenue /
-- CR arrears_liability), so the reversal conserves by construction: the revenue
-- recognised at accrual is given back, and the liability returns to zero.
-- Distinct from owed_payment, which means a rail actually collected money.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.ledger_transfers
    DROP CONSTRAINT ledger_transfers_type_check;

ALTER TABLE openrails.ledger_transfers
    ADD CONSTRAINT ledger_transfers_type_check CHECK ((transfer_type = ANY (ARRAY[
        'deposit'::text,
        'credit_spend'::text,
        'credit_expire'::text,
        'credit_revoke'::text,
        'credit_reinstate'::text,
        'owed_accrual'::text,
        'owed_payment'::text,
        'owed_writeoff'::text
    ]))) NOT VALID;
