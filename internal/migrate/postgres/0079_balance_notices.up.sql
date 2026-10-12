-- Repair: none-needed The new columns start null or zero, and the backfill writes only rows it creates.
-- Every prepaid balance is watched, not only a running charge's: a null
-- next_check_at is a watch nothing has made due.
ALTER TABLE billing.balance_watches
    ALTER COLUMN next_check_at DROP NOT NULL,
    ADD COLUMN funded_at timestamp with time zone,
    ADD COLUMN usage_rate_per_hour bigint NOT NULL DEFAULT 0,
    ADD COLUMN rate_sampled_at timestamp with time zone,
    ADD CONSTRAINT balance_watches_usage_rate_per_hour_check CHECK (usage_rate_per_hour >= 0);
ALTER TABLE billing.balance_watches ALTER COLUMN usage_rate_per_hour DROP DEFAULT;

DROP INDEX billing.balance_watches_next_check_at_idx;
CREATE INDEX balance_watches_next_check_at_idx ON billing.balance_watches USING btree (next_check_at, merchant_id, customer_id, currency)
    WHERE next_check_at IS NOT NULL;

INSERT INTO billing.balance_watches (merchant_id, customer_id, currency, next_check_at, usage_rate_per_hour, updated_at)
SELECT a.merchant_id, a.customer_id, a.currency, NULL, 0, now()
FROM billing.ledger_accounts a
LEFT JOIN billing.money_settings s
  ON s.merchant_id = a.merchant_id AND s.customer_id = a.customer_id AND s.currency = a.currency
WHERE a.account_type = 'customer_balance' AND a.customer_id IS NOT NULL
  AND (s.billing_mode IS DISTINCT FROM 'arrears' OR s.collection_revoked_at IS NOT NULL)
ON CONFLICT (merchant_id, customer_id, currency) DO NOTHING;

COMMENT ON TABLE billing.balance_watches IS 'One row per prepaid customer balance and per customer and currency with a running charge: when its balance is next looked at (a low line, the run-out, a credit lot lapse or the next 00:00 UTC, whichever is first; null until a change marks it due) and what the last look found. Derived from the balance, the running charges and the billing mode; an arrears customer with no running charge has none.';
COMMENT ON COLUMN billing.balance_watches.next_check_at IS 'When the balance is next looked at; to_timestamp(0) once a change marks it due, null with nothing scheduled.';
COMMENT ON COLUMN billing.balance_watches.depletes_at IS 'When the customer''s capacity reaches zero at the rate of its running charges and the lapse of its credit lots, as last computed; in the past while it is at zero, null when nothing exact moves it.';
COMMENT ON COLUMN billing.balance_watches.low_notified_at IS 'When balance.low was sent for the current crossing; cleared by a look that finds the balance above its line after a funding.';
COMMENT ON COLUMN billing.balance_watches.depleted_at IS 'When balance.depleted was sent for the current crossing; cleared by a look that finds capacity above zero after a funding.';
COMMENT ON COLUMN billing.balance_watches.funded_at IS 'When the customer was funded (a deposit, a credit grant, a reinstatement or a payment of what it owes) since its last notice; only funding re-arms a notice, and a notice clears it.';
COMMENT ON COLUMN billing.balance_watches.usage_rate_per_hour IS 'Exponentially weighted rate of usage spend, one-hour time constant, in native units per hour: each look adds the usage events dated since rate_sampled_at.';
COMMENT ON COLUMN billing.balance_watches.rate_sampled_at IS 'The instant usage_rate_per_hour was sampled at; usage events dated before it are not read again.';

-- A customer's own low-balance line in a currency; null inherits the merchant's.
ALTER TABLE billing.money_settings
    ADD COLUMN low_balance_amount bigint,
    ADD COLUMN low_balance_runway_hours integer,
    ADD CONSTRAINT money_settings_low_balance_amount_check CHECK (low_balance_amount >= 0),
    ADD CONSTRAINT money_settings_low_balance_runway_hours_check CHECK (low_balance_runway_hours >= 0 AND low_balance_runway_hours <= 8760);
COMMENT ON COLUMN billing.money_settings.low_balance_amount IS 'The customer''s low-balance amount line in the row currency; 0 is none, null inherits the merchant''s settings.balance_notices.';
COMMENT ON COLUMN billing.money_settings.low_balance_runway_hours IS 'The customer''s low-balance runway line in hours of spend at its burn rate; 0 is none, null inherits the merchant''s settings.balance_notices, else 24.';

-- The customer's line replaces the running charge's.
ALTER TABLE billing.running_charges DROP COLUMN low_balance_amount;

COMMENT ON TABLE billing.host_outbox IS 'Typed durable host events: successful rail payment settlements, delinquency lifecycle transitions, product key changes, order transitions, customer links, revoked charge agreements and the low and depleted balance of a watched customer. Acknowledge after idempotent processing; acknowledgments are separate from notification read state. Retention: delivered events are deleted 30 days after delivered_at; an undelivered event is never deleted.';
