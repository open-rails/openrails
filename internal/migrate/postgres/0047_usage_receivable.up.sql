-- A chargeback of a usage order's payment owes again what it paid: the units
-- book's owed_reopened (DR customer_owed / CR units_sold), the inverse of
-- owed_payment.
ALTER TABLE billing.ledger_transfers DROP CONSTRAINT ledger_transfers_type_check;
ALTER TABLE billing.ledger_transfers ADD CONSTRAINT ledger_transfers_type_check CHECK (
    book = 'units' AND transfer_type IN ('deposit', 'deposit_bonus', 'credit_purchase_revenue', 'credit_spend', 'credit_expire', 'credit_revoke', 'credit_reinstate', 'owed_accrual', 'owed_payment', 'owed_writeoff', 'owed_repayment', 'owed_reopened', 'credit_refund', 'credit_refund_restore', 'credit_refund_cash', 'credit_refund_cash_restore', 'credit_refund_funding', 'usage_reversed')
    OR book = 'general' AND transfer_type IN ('cash_received', 'cash_applied', 'cash_returned', 'revenue_adjusted', 'revenue_recognized', 'refund_deferred', 'refund_recognized', 'chargeback_deferred', 'chargeback_recognized', 'dispute_won', 'credit_released', 'credit_breakage', 'credit_forfeited', 'credit_forfeit_reversed', 'promotional_credit_applied', 'usage_accrued', 'usage_collected', 'usage_repaid', 'usage_waived', 'usage_reopened', 'bad_debt', 'bad_debt_recovery', 'processor_fee', 'chargeback_fee', 'opening_balance', 'usage_reversed'));
