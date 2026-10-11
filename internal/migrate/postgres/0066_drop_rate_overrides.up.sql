-- parent: 65 sha256:ba76508cd8dfc4330c0dbf794c325870cd5f7f81aa9ab8e15968c83014e11986
-- A meter's rate card prices every customer: customer rate overrides go, with
-- the customer column that scoped them and the revision only they carried.
-- Every remaining card belongs to a product.
DELETE FROM billing.catalog_rate_cards WHERE customer_id IS NOT NULL;

DROP INDEX billing.catalog_rate_cards_customer_id_meter_key_key;
DROP INDEX billing.catalog_rate_cards_meter_key_customer_id_idx;
DROP INDEX billing.catalog_rate_cards_meter_key_key;
ALTER TABLE billing.catalog_rate_cards DROP CONSTRAINT catalog_rate_cards_product_scope_check;
ALTER TABLE billing.catalog_rate_cards DROP CONSTRAINT catalog_rate_cards_customer_id_fkey;
ALTER TABLE billing.catalog_rate_cards DROP COLUMN customer_id;
ALTER TABLE billing.catalog_rate_cards ALTER COLUMN product_id SET NOT NULL;
CREATE UNIQUE INDEX catalog_rate_cards_meter_key_key ON billing.catalog_rate_cards USING btree (merchant_id, meter_key) WHERE (meter_key IS NOT NULL);

DROP TRIGGER catalog_rate_card_revision ON billing.catalog_rate_cards;
ALTER TABLE billing.catalog_rate_cards DROP CONSTRAINT catalog_rate_cards_revision_check;
ALTER TABLE billing.catalog_rate_cards DROP COLUMN revision;
