-- Repair: none-needed A new nullable column and new indexes, armed below from
-- the due records they replace.
-- A usage account is checked at usage_due_at: its bill date, or at once when
-- spending crossed its threshold, an admission asked to collect, or the
-- merchant's usage terms changed. One cross-merchant read finds the due ones.
ALTER TABLE billing.money_settings ADD COLUMN usage_due_at timestamptz;
DROP INDEX billing.money_settings_usage_due;
CREATE INDEX money_settings_usage_due_at_idx ON billing.money_settings (usage_due_at, merchant_id, customer_id, currency)
    WHERE usage_due_at IS NOT NULL;
COMMENT ON COLUMN billing.money_settings.usage_due_at IS 'When the usage account is next checked: its next bill date, or now once spending crossed its threshold, an admission asked to collect or the merchant''s usage terms changed. Null: nothing is pending.';
COMMENT ON COLUMN billing.money_settings.usage_next_bill_at IS 'Next monthly usage bill date; null while nothing is pending, set again from the next accrual. A below-minimum balance advances it while its unbilled accruals carry forward.';
UPDATE billing.money_settings SET usage_due_at = CASE WHEN usage_collection_due THEN now() ELSE usage_next_bill_at END
WHERE usage_collection_due OR usage_next_bill_at IS NOT NULL;
UPDATE billing.money_settings s SET usage_due_at = now()
WHERE s.usage_due_at IS NULL AND EXISTS (SELECT 1 FROM billing.owed_items oi
    WHERE oi.merchant_id = s.merchant_id AND oi.customer_id = s.customer_id AND oi.currency = s.currency
      AND oi.status = 'pending' AND oi.order_id IS NULL);

-- An automatically collected usage order is first charged at its due date:
-- next_collection_attempt_at is set when it opens, so every collection is one
-- indexed instant.
DROP INDEX billing.orders_usage_collection_idx;
CREATE INDEX orders_usage_collection_due_idx ON billing.orders (next_collection_attempt_at, merchant_id, id)
    WHERE origin = 'usage' AND status = 'open' AND collection_method = 'charge_automatically' AND next_collection_attempt_at IS NOT NULL;
UPDATE billing.orders SET next_collection_attempt_at = due_at
WHERE origin = 'usage' AND status = 'open' AND collection_method = 'charge_automatically'
  AND collection_failure_count = 0 AND next_collection_attempt_at IS NULL AND amount_paid < total;
COMMENT ON COLUMN billing.orders.next_collection_attempt_at IS 'When a usage order''s next scheduled charge runs: its due date when it opens, then the retry schedule''s; NULL while none is scheduled (stopped until the customer designates a card).';
