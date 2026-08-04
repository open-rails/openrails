SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE INDEX idx_payment_settlement_events_delivered
    ON openrails.payment_settlement_events (merchant_id, delivered_at)
    WHERE delivered_at IS NOT NULL;

CREATE INDEX idx_grants_credit_customer_currency
    ON openrails.grants (merchant_id, customer_id, currency, starts_at, ends_at)
    WHERE kind = 'credit' AND event = 'grant';
