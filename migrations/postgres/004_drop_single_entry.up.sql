-- =============================================================================
-- #512 / #514 HARD CUT — drop the single-entry money tables.
--
-- Balance is now DERIVED from the #512 double-entry ledger (ledger_accounts +
-- ledger_transfers, migration 002); credit lots ARE #514 grants (migration 003);
-- arrears is the arrears_liability ledger account. The single-entry,
-- mutate-in-place money_transactions (with its denormalized balance_after) and
-- the mutable money_blocks credit-lot table are no longer read or written by any
-- code path, so they are dropped outright (pre-launch: no data migration).
--
-- money_blocks carries the only inbound FK to money_transactions
-- (money_blocks.source_transaction_id), so dropping money_blocks first keeps the
-- drop self-contained. invoice_payments.money_transaction_id and
-- usage_events.money_transaction_id are plain uuid columns (no FK) that now hold
-- a ledger_transfers id, so they need no change.
-- =============================================================================

DROP TABLE IF EXISTS openrails.money_blocks;
DROP TABLE IF EXISTS openrails.money_transactions;
