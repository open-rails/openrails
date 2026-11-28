-- Nothing wrote a payment's credit grant snapshot: a purchased lot's promise
-- is its invoice line's frozen credit_grant.
ALTER TABLE billing.payments DROP COLUMN credit_grant_snapshot;
