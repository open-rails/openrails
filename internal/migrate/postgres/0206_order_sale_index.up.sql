-- Repair: none-needed. A new index; no row changes.
-- An engine renewal finds the sale that paid an order's first period by the
-- order it paid: one index read, not every past sale of the merchant.
CREATE INDEX provider_intents_order_sale_idx ON billing.provider_intents (merchant_id, (payload->>'order_id'))
    WHERE intent_type = 'nmi_sale' AND status = 'succeeded' AND payload ? 'order_id';
