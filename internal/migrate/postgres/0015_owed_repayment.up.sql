-- parent: 14 sha256:fffb9de8de04d77debe61c57b63f49a4b2b417fc4ca58028f942bc5b8acca035
-- Repair: none-needed Both vocabularies strictly expand; no existing transfer or
-- invoice payment can violate the added members.
ALTER TABLE billing.ledger_transfers DROP CONSTRAINT ledger_transfers_type_check;
ALTER TABLE billing.ledger_transfers ADD CONSTRAINT ledger_transfers_type_check CHECK ((transfer_type = ANY (ARRAY['deposit'::text, 'deposit_bonus'::text, 'credit_purchase_revenue'::text, 'credit_spend'::text, 'credit_expire'::text, 'credit_revoke'::text, 'credit_reinstate'::text, 'owed_accrual'::text, 'owed_payment'::text, 'owed_writeoff'::text, 'owed_repayment'::text, 'credit_refund'::text, 'credit_refund_restore'::text, 'credit_refund_cash'::text, 'credit_refund_cash_restore'::text, 'credit_refund_funding'::text])));

-- A balance payment settles an invoice from the customer's funded balance.
ALTER TABLE billing.invoice_payments DROP CONSTRAINT invoice_payments_channel_check;
ALTER TABLE billing.invoice_payments ADD CONSTRAINT invoice_payments_channel_check CHECK ((channel = ANY (ARRAY['rail'::text, 'manual'::text, 'balance'::text])));
