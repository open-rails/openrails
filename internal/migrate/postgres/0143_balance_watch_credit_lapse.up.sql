-- Repair: none-needed The watch, not an hourly sweep, expires a lapsed credit
-- lot: every balance holding a lot not yet expired that lapses is watched, an
-- arrears one too, and looked at on the next pass, which schedules the lapse.
INSERT INTO billing.balance_watches (merchant_id, customer_id, currency, next_check_at, usage_rate_per_hour)
SELECT DISTINCT g.merchant_id, g.customer_id, g.currency, to_timestamp(0), 0
FROM billing.grants g
WHERE g.kind = 'credit' AND g.event = 'grant' AND g.ends_at IS NOT NULL AND g.currency IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM billing.grants t WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede'))
  AND NOT EXISTS (SELECT 1 FROM billing.ledger_transfers lt WHERE lt.merchant_id = g.merchant_id AND lt.grant_id = g.id AND lt.transfer_type = 'credit_expire')
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET next_check_at = to_timestamp(0);

COMMENT ON TABLE billing.balance_watches IS 'One row per prepaid customer balance, per customer and currency with a running charge and per balance holding a credit lot that lapses: when its balance is next looked at (a low line, the run-out, a credit lot''s lapse or the next 00:00 UTC, whichever is first; null until a change marks it due) and what the last look found. The look at a lapse expires the lot. Derived from the balance, the running charges, the credit lots and the billing mode; an arrears customer with neither a running charge nor a lot that lapses has none.';
