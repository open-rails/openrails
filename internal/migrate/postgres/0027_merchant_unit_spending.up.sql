-- A merchant's own units (credits, tokens, an in-game currency) are bought
-- with money, given and spent. They are prepaid, never owed: usage its lots
-- cannot take is forgiven; an order in a unit is paid from the customer's
-- balance, never by a payment; and what a pack of a unit cost is money, in a
-- registry currency.

-- The registry's currencies (internal/currency); every other code a balance
-- may count is a merchant's unit.
ALTER TABLE billing.orders DROP CONSTRAINT orders_complete_payment_check;
ALTER TABLE billing.orders ADD CONSTRAINT orders_complete_payment_check CHECK (status <> 'complete' OR total = 0 OR payment_id IS NOT NULL OR origin = 'usage' OR collection_owner <> 'engine'
    OR currency NOT IN ('AED', 'AUD', 'BRL', 'CAD', 'CHF', 'CNY', 'CZK', 'DKK', 'EUR', 'GBP', 'HKD', 'ILS', 'INR', 'JPY', 'KRW', 'MXN', 'MYR', 'NOK', 'NZD', 'PHP', 'PLN', 'PYUSD', 'SAR', 'SEK', 'SGD', 'SOL', 'THB', 'TRY', 'USD', 'USD1', 'USDC', 'USDG', 'USDT', 'ZAR'));
COMMENT ON CONSTRAINT orders_complete_payment_check ON billing.orders IS 'A complete order was paid by its payment, unless it was free, is a usage order, is collected by a provider, or is in a merchant''s unit: the customer''s balance paid it, in credit_spend legs naming the order.';

ALTER TABLE billing.ledger_accounts ADD CONSTRAINT ledger_accounts_owed_currency_check CHECK (account_type <> 'customer_owed'
    OR currency IN ('AED', 'AUD', 'BRL', 'CAD', 'CHF', 'CNY', 'CZK', 'DKK', 'EUR', 'GBP', 'HKD', 'ILS', 'INR', 'JPY', 'KRW', 'MXN', 'MYR', 'NOK', 'NZD', 'PHP', 'PLN', 'PYUSD', 'SAR', 'SEK', 'SGD', 'SOL', 'THB', 'TRY', 'USD', 'USD1', 'USDC', 'USDG', 'USDT', 'ZAR'));
COMMENT ON CONSTRAINT ledger_accounts_owed_currency_check ON billing.ledger_accounts IS 'Nothing is owed in a merchant''s unit: an arrears account holds a registry currency.';

ALTER TABLE billing.credit_grants ADD CONSTRAINT credit_grants_paid_currency_registry_check CHECK (paid_currency IS NULL
    OR paid_currency IN ('AED', 'AUD', 'BRL', 'CAD', 'CHF', 'CNY', 'CZK', 'DKK', 'EUR', 'GBP', 'HKD', 'ILS', 'INR', 'JPY', 'KRW', 'MXN', 'MYR', 'NOK', 'NZD', 'PHP', 'PLN', 'PYUSD', 'SAR', 'SEK', 'SGD', 'SOL', 'THB', 'TRY', 'USD', 'USD1', 'USDC', 'USDG', 'USDT', 'ZAR'));
COMMENT ON COLUMN billing.credit_grants.paid_amount IS 'What was paid for the lot, in paid_currency (a registry currency); NULL for a gift. A pack of a merchant''s unit is paid in any currency: its rate is paid_amount over units, bonus units included.';

-- A pack of a merchant's unit has no cash leg in the unit: what returned of
-- its payment is the payment's net settled reversals, up to what it paid.
CREATE OR REPLACE FUNCTION billing.credit_grant_reservation(p_merchant uuid, p_lot uuid) RETURNS bigint
LANGUAGE sql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
    SELECT COALESCE((
        SELECT LEAST(g.units, ceil(g.units::numeric * GREATEST(LEAST(pending.amount, g.paid_amount - returned.amount), 0) / g.paid_amount))::bigint
        FROM billing.credit_grants g
        CROSS JOIN LATERAL (SELECT sum(-r.amount)::numeric AS amount FROM billing.payments r
            WHERE r.merchant_id = g.merchant_id AND r.refunded_payment_id = g.payment_id AND r.currency = g.paid_currency
              AND r.status = 'pending' AND r.amount < 0 AND r.deleted_at IS NULL) pending
        CROSS JOIN LATERAL (SELECT CASE WHEN g.paid_currency = g.unit THEN
                (SELECT COALESCE(sum(CASE t.transfer_type WHEN 'credit_refund_cash' THEN t.amount WHEN 'credit_refund_cash_restore' THEN -t.amount ELSE 0 END), 0)::numeric
                 FROM billing.ledger_transfers t
                 WHERE t.merchant_id = g.merchant_id AND t.credit_grant_id = g.id AND t.operation = 'purchased_credit_refund')
            ELSE
                (SELECT LEAST(GREATEST(COALESCE(sum(-r.amount), 0), 0), g.paid_amount)::numeric
                 FROM billing.payments r
                 WHERE r.merchant_id = g.merchant_id AND r.refunded_payment_id = g.payment_id
                   AND r.status = 'succeeded' AND r.deleted_at IS NULL)
            END AS amount) returned
        WHERE g.merchant_id = p_merchant AND g.id = p_lot AND g.payment_id IS NOT NULL AND g.paid_amount IS NOT NULL
          AND pending.amount > 0), 0)
$$;

-- A posting of usage in a merchant's unit forgives what the lots cannot take,
-- and a reversal takes back forgiveness before it credits.
ALTER TABLE billing.usage_postings ADD COLUMN forgiven bigint NOT NULL DEFAULT 0;
ALTER TABLE billing.usage_postings ALTER COLUMN forgiven DROP DEFAULT;
ALTER TABLE billing.usage_postings ADD CONSTRAINT usage_postings_forgiven_check CHECK (forgiven = 0
    OR currency NOT IN ('AED', 'AUD', 'BRL', 'CAD', 'CHF', 'CNY', 'CZK', 'DKK', 'EUR', 'GBP', 'HKD', 'ILS', 'INR', 'JPY', 'KRW', 'MXN', 'MYR', 'NOK', 'NZD', 'PHP', 'PLN', 'PYUSD', 'SAR', 'SEK', 'SGD', 'SOL', 'THB', 'TRY', 'USD', 'USD1', 'USDC', 'USDG', 'USDT', 'ZAR')
    AND (reason <> 'reversal' AND forgiven > 0 AND forgiven <= amount OR reason = 'reversal' AND forgiven < 0 AND forgiven >= amount));
COMMENT ON COLUMN billing.usage_postings.forgiven IS 'Of amount, what the ledger did not take: usage in a merchant''s unit its lots could not cover, never owed (positive); a reversal''s share that took back forgiveness rather than credit (negative). Zero in a registry currency.';
CREATE INDEX usage_postings_forgiven_idx ON billing.usage_postings (merchant_id, cut_at) WHERE forgiven <> 0;
