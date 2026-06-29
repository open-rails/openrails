-- #603: fiat money values stored in Postgres move from provider minor units
-- (USD cents) to OpenRails internal micros (1 major unit = 1,000,000).
-- Provider-facing snapshots remain provider-native at the rail boundary.

UPDATE openrails.checkout_sessions SET amount = amount * 10000;

UPDATE openrails.grants SET amount = amount * 10000 WHERE amount IS NOT NULL;

UPDATE openrails.invoice_items
SET unit_amount = unit_amount * 10000,
    amount = amount * 10000;

UPDATE openrails.invoice_payments SET amount = amount * 10000;

UPDATE openrails.invoices
SET usage_total = usage_total * 10000,
    deposits_total = deposits_total * 10000,
    owed_accrued = owed_accrued * 10000,
    owed_paid = owed_paid * 10000,
    closing_balance = closing_balance * 10000,
    subtotal_amount = subtotal_amount * 10000,
    total_amount = total_amount * 10000,
    amount_paid = amount_paid * 10000,
    amount_due = amount_due * 10000;

UPDATE openrails.ledger_accounts
SET credits_posted = credits_posted * 10000,
    debits_posted = debits_posted * 10000;

UPDATE openrails.ledger_transfers
SET amount = amount * 10000,
    allow_debit_negative_up_to = allow_debit_negative_up_to * 10000;

UPDATE openrails.money_settings
SET max_spend_per_day = CASE WHEN max_spend_per_day IS NULL THEN NULL ELSE max_spend_per_day * 10000 END,
    max_spend_per_month = CASE WHEN max_spend_per_month IS NULL THEN NULL ELSE max_spend_per_month * 10000 END,
    max_outstanding_owed_amount = CASE WHEN max_outstanding_owed_amount IS NULL THEN NULL ELSE max_outstanding_owed_amount * 10000 END,
    low_balance_threshold = CASE WHEN low_balance_threshold IS NULL THEN NULL ELSE low_balance_threshold * 10000 END,
    auto_topup_amount_cents = CASE WHEN auto_topup_amount_cents IS NULL THEN NULL ELSE auto_topup_amount_cents * 10000 END,
    outstanding_owed_amount = outstanding_owed_amount * 10000,
    credit_limit_amount = credit_limit_amount * 10000;

UPDATE openrails.payments
SET amount = amount * 10000,
    list_amount = list_amount * 10000;

UPDATE openrails.prices
SET amount = amount * 10000,
    initial_amount = initial_amount * 10000
WHERE true;

UPDATE openrails.usage_events SET amount = amount * 10000;

COMMENT ON COLUMN openrails.prices.amount IS 'Price amount in row currency micros (1 major unit = 1,000,000).';
COMMENT ON COLUMN openrails.prices.initial_amount IS '#602 intro/trial: first-period price in row currency micros; 0 = free trial; NULL = flat price.';
