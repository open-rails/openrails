DROP INDEX openrails.idx_ledger_transfers_owed_accrual_once;

CREATE UNIQUE INDEX idx_ledger_transfers_owed_accrual_once
    ON openrails.ledger_transfers (merchant_id, customer_id, currency, source, source_id)
    WHERE transfer_type = 'owed_accrual' AND source_id <> '';

DROP INDEX openrails.idx_ledger_transfers_credit_spend_once;
