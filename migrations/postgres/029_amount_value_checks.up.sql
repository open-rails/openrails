-- #(advisor-008) Guard core monetary columns at the DB layer, matching the
-- existing invoice/usage amount CHECKs. Predicate chosen per Step 1 evidence.
--
-- PRICES ONLY — payments columns are intentionally excluded:
--   payments.amount    : refund rows are stored as NEGATIVE amounts (see grants.sql:197,
--                        payments.sql:110, reconciliation.sql:314-315). A >= 0 CHECK
--                        would reject every refund row. NO CHECK added.
--   payments.list_amount: ReconcileRecordRefund sets list_amount = amount (negative) for
--                        refund rows (reconciliation.sql:324). NO CHECK added.
--
-- prices.amount: no evidence of negative usage; >= 0 allows $0 free-tier prices.

ALTER TABLE openrails.prices
    ADD CONSTRAINT prices_amount_nonneg_chk CHECK (amount >= 0);
