-- parent: 4 sha256:3b3a7a0b7b01937cc9e4f15830ae3b4d61c97e6904ac86e59421eb722ada33a1
-- Purchased credit lots retain one grant per payment, with explicit cash and
-- merchant promotional funding. Existing manual grants remain unchanged.
-- Repair: none-needed Existing credit writers never emitted paid_amount provenance.
-- Imported/manual payment-linked credit lots are outside this native-fulfillment
-- identity. Preserve every money fact; reject already-marked duplicates explicitly.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM billing.grants
        WHERE kind='credit' AND event='grant' AND payment_id IS NOT NULL
          AND spec_snapshot->'deposit'->'paid_amount' IS NOT NULL
        GROUP BY merchant_id,payment_id HAVING count(*)>1) THEN
        RAISE EXCEPTION 'native purchased credits contain multiple grants for one payment'
          USING HINT='Inspect billing.grants grouped by merchant_id,payment_id where kind=credit, event=grant and deposit.paid_amount is present. Preserve all grant and ledger facts; reconcile duplicate fulfillment before retrying migration.';
    END IF;
END $$;
CREATE UNIQUE INDEX grants_purchased_credit_payment_key
ON billing.grants (merchant_id, payment_id)
WHERE kind='credit' AND event='grant' AND payment_id IS NOT NULL
  AND spec_snapshot->'deposit'->'paid_amount' IS NOT NULL;

-- Repair: none-needed Ledger vocabularies strictly expand existing account and
-- transfer values. Lot-once identity retains every old member and adds only new
-- transfer kinds; no existing transfer can violate the added members.
ALTER TABLE billing.ledger_accounts DROP CONSTRAINT ledger_accounts_type_check;
ALTER TABLE billing.ledger_accounts ADD CONSTRAINT ledger_accounts_type_check CHECK ((account_type = ANY (ARRAY['customer_balance'::text, 'platform_revenue'::text, 'processor_clearing'::text, 'arrears_liability'::text, 'expired_credits'::text, 'revoked_credits'::text, 'promotional_funding'::text, 'credit_refund_clearing'::text, 'credit_refund_loss'::text])));
ALTER TABLE billing.ledger_transfers DROP CONSTRAINT ledger_transfers_type_check;
ALTER TABLE billing.ledger_transfers ADD CONSTRAINT ledger_transfers_type_check CHECK ((transfer_type = ANY (ARRAY['deposit'::text, 'deposit_bonus'::text, 'credit_purchase_revenue'::text, 'credit_spend'::text, 'credit_expire'::text, 'credit_revoke'::text, 'credit_reinstate'::text, 'owed_accrual'::text, 'owed_payment'::text, 'owed_writeoff'::text, 'credit_refund'::text, 'credit_refund_restore'::text, 'credit_refund_cash'::text, 'credit_refund_cash_restore'::text, 'credit_refund_funding'::text])));
DROP INDEX billing.ledger_transfers_grant_id_transfer_type_key;
CREATE UNIQUE INDEX ledger_transfers_grant_id_transfer_type_key ON billing.ledger_transfers USING btree (merchant_id, grant_id, transfer_type) WHERE ((grant_id IS NOT NULL) AND (transfer_type = ANY (ARRAY['deposit'::text, 'deposit_bonus'::text, 'credit_purchase_revenue'::text, 'credit_expire'::text, 'credit_revoke'::text])));

-- Spending reads only the small set of pending credit-refund reservations.
CREATE INDEX payments_pending_credit_refunds_idx
ON billing.payments (merchant_id, customer_id, currency, refunded_payment_id)
WHERE status='pending' AND amount<0 AND deleted_at IS NULL;
