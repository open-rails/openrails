-- Nothing posts deposit_bonus, credit_purchase_revenue, credit_refund_cash or
-- credit_refund_cash_restore: the units book's vocabulary and its lot-once
-- index drop them.

ALTER TABLE billing.ledger_transfers DROP CONSTRAINT ledger_transfers_type_check;
ALTER TABLE billing.ledger_transfers ADD CONSTRAINT ledger_transfers_type_check CHECK (
    book = 'units' AND transfer_type IN ('deposit', 'credit_spend', 'credit_expire', 'credit_revoke', 'credit_reinstate', 'owed_accrual', 'owed_payment', 'owed_writeoff', 'owed_repayment', 'owed_reopened', 'credit_refund', 'credit_refund_restore', 'credit_refund_funding', 'usage_reversed')
    OR book = 'general' AND transfer_type IN ('cash_received', 'cash_applied', 'cash_returned', 'revenue_adjusted', 'revenue_recognized', 'refund_deferred', 'refund_recognized', 'chargeback_deferred', 'chargeback_recognized', 'dispute_won', 'credit_released', 'credit_breakage', 'credit_forfeited', 'credit_forfeit_reversed', 'promotional_credit_applied', 'usage_accrued', 'usage_collected', 'usage_repaid', 'usage_waived', 'usage_reopened', 'bad_debt', 'bad_debt_recovery', 'processor_fee', 'chargeback_fee', 'opening_balance', 'usage_reversed'));

DROP INDEX billing.ledger_transfers_credit_grant_id_transfer_type_key;
CREATE UNIQUE INDEX ledger_transfers_credit_grant_id_transfer_type_key ON billing.ledger_transfers (merchant_id, credit_grant_id, transfer_type)
    WHERE credit_grant_id IS NOT NULL AND transfer_type IN ('deposit', 'credit_expire');
