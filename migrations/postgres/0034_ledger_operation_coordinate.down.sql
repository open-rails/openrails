SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP INDEX openrails.idx_ledger_transfers_owed_accrual_once;
DROP INDEX openrails.idx_ledger_transfers_credit_spend_once;

ALTER TABLE openrails.ledger_transfers DROP COLUMN operation;

CREATE UNIQUE INDEX idx_ledger_transfers_credit_spend_once
    ON openrails.ledger_transfers (merchant_id, customer_id, currency, source, source_id, grant_id)
    WHERE transfer_type = 'credit_spend';

CREATE UNIQUE INDEX idx_ledger_transfers_owed_accrual_once
    ON openrails.ledger_transfers (merchant_id, customer_id, currency, source, source_id)
    WHERE transfer_type = 'owed_accrual';
