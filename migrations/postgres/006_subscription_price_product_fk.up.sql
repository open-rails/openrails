-- Enforce subscription.price_id -> prices(id, product_id, merchant_id), making
-- subscription price/product mismatches structurally impossible.

UPDATE openrails.subscriptions AS sub
SET product_id = price.product_id,
    updated_at = now()
FROM openrails.prices AS price
WHERE sub.price_id = price.id
  AND sub.merchant_id = price.merchant_id
  AND sub.product_id <> price.product_id;

CREATE UNIQUE INDEX IF NOT EXISTS uq_prices_id_product_merchant
    ON openrails.prices USING btree (id, product_id, merchant_id);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'subscriptions_price_product_merchant_fkey'
          AND conrelid = 'openrails.subscriptions'::regclass
    ) THEN
        ALTER TABLE openrails.subscriptions
            ADD CONSTRAINT subscriptions_price_product_merchant_fkey
            FOREIGN KEY (price_id, product_id, merchant_id)
            REFERENCES openrails.prices(id, product_id, merchant_id)
            NOT VALID;
    END IF;
END
$$;

ALTER TABLE openrails.subscriptions
    VALIDATE CONSTRAINT subscriptions_price_product_merchant_fkey;
