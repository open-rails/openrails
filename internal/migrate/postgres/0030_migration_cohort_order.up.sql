-- A price migration walks its subscribers a page at a time in (price, id)
-- order.
DROP INDEX billing.subscriptions_price_id_idx;
CREATE INDEX subscriptions_price_id_idx ON billing.subscriptions (merchant_id, price_id, id);
