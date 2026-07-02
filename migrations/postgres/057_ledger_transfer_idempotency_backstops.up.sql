-- #677 DB backstops for ledger-transfer idempotency. All transfer dedupe is
-- app-side (idx_ledger_transfers_source is non-unique); these make the
-- can't-happen-twice writes structurally unique so a lock bypass or overlapping
-- repair errors instead of double-posting.

-- A #514 credit lot is deposited once, expired once, clawed back once
-- (credit_spend repeats per lot and is excluded).
CREATE UNIQUE INDEX idx_ledger_transfers_lot_once
    ON openrails.ledger_transfers (merchant_id, grant_id, transfer_type)
    WHERE grant_id IS NOT NULL
      AND transfer_type IN ('deposit', 'credit_expire', 'credit_revoke');

-- An owed accrual posts once per operation coordinate. Empty source_id is
-- excluded: SpendCredits permits a keyless spend whose owed spill repeats.
CREATE UNIQUE INDEX idx_ledger_transfers_owed_accrual_once
    ON openrails.ledger_transfers (merchant_id, customer_id, currency, source, source_id)
    WHERE transfer_type = 'owed_accrual' AND source_id <> '';
