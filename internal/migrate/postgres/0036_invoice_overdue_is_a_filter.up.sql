-- parent: 35 sha256:b2ea19ed4e55bfa9e6802b6c47149b0a512178cc7fb32d570c32543dc64d71f5
-- Repair: none-needed Every past_due invoice becomes open before the check narrows: overdue is read from due_at.
-- An invoice is overdue by its due date, not by a status: past_due goes, and
-- the partial indexes name the one open status.
UPDATE billing.invoices SET status = 'open' WHERE status = 'past_due';
ALTER TABLE billing.invoices DROP CONSTRAINT invoices_status_check;
ALTER TABLE billing.invoices ADD CONSTRAINT invoices_status_check
    CHECK (status IN ('draft', 'open', 'paid', 'voided', 'uncollectible'));
DROP INDEX billing.invoices_next_collection_attempt_at_due_at_idx;
CREATE INDEX invoices_next_collection_attempt_at_due_at_idx ON billing.invoices USING btree (merchant_id, next_collection_attempt_at, due_at)
    WHERE status = 'open' AND amount_due > 0 AND collection_method = 'charge_automatically';
DROP INDEX billing.invoices_customer_id_currency_due_at_idx;
CREATE INDEX invoices_customer_id_currency_due_at_idx ON billing.invoices USING btree (merchant_id, customer_id, currency, due_at)
    WHERE status = 'open' AND amount_due > 0;
DROP INDEX billing.invoices_awaiting_period_starts_at_idx;
CREATE INDEX invoices_awaiting_period_starts_at_idx ON billing.invoices USING btree (merchant_id, customer_id, currency, period_starts_at)
    WHERE status IN ('open', 'uncollectible') AND (amount_paid + amount_due) < total_amount;
