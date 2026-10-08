-- parent: 2 sha256:c04092234322616c50b0c9fe7f3d3b6e0b85adc9372507f9ae95eb907641b9ac
-- Repair: none-needed The existing live (merchant_id,key) uniqueness and financial tuple uniqueness imply the weaker product/key indexes. New price revisions are backfilled below before their constraints; product revisions start at the valid default zero.
-- A price key names a version chain within its product, not the whole merchant.
DROP INDEX billing.prices_key_key;
CREATE UNIQUE INDEX prices_key_key ON billing.prices (merchant_id, product_id, key) WHERE NOT archived;
DROP INDEX billing.prices_key_idx;
CREATE INDEX prices_key_idx ON billing.prices (merchant_id, product_id, key);
COMMENT ON COLUMN billing.prices.key IS 'Product-local handle for an immutable price version chain. One live row per (merchant_id, product_id, key); archived versions retain their key and product.';

-- Existing IDs and all purchase references stay intact. A revision identifies
-- immutable terms, so reactivating old terms keeps their original revision.
ALTER TABLE billing.prices ADD COLUMN revision bigint;
ALTER TABLE billing.prices DISABLE TRIGGER catalog_authored_price;
WITH numbered AS (
    SELECT merchant_id, id, row_number() OVER (PARTITION BY merchant_id, product_id, key ORDER BY created_at, id) - 1 AS revision
    FROM billing.prices
)
UPDATE billing.prices price SET revision=numbered.revision
FROM numbered WHERE price.merchant_id=numbered.merchant_id AND price.id=numbered.id;
ALTER TABLE billing.prices ENABLE TRIGGER catalog_authored_price;
ALTER TABLE billing.prices ALTER COLUMN revision SET NOT NULL;
ALTER TABLE billing.prices ADD CONSTRAINT prices_revision_nonnegative CHECK (revision >= 0);
ALTER TABLE billing.prices ADD CONSTRAINT prices_product_key_revision_key UNIQUE (merchant_id, product_id, key, revision);
ALTER TABLE billing.prices DROP CONSTRAINT prices_product_amount_window_key;
ALTER TABLE billing.prices ADD CONSTRAINT prices_product_amount_window_key
    UNIQUE NULLS NOT DISTINCT (merchant_id, product_id, key, amount, currency, access_duration_hours, auto_renew, trial_unit_amount, trial_duration_hours);

-- catalog_authored_price has already locked the merchant when this trigger
-- runs. Restores carry the original revision explicitly; ordinary inserts omit it.
CREATE FUNCTION billing.assign_price_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.revision IS NULL THEN
        SELECT COALESCE(MAX(revision) + 1, 0) INTO NEW.revision FROM billing.prices
        WHERE merchant_id=NEW.merchant_id AND product_id=NEW.product_id AND key=NEW.key;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER price_revision BEFORE INSERT ON billing.prices
FOR EACH ROW EXECUTE FUNCTION billing.assign_price_revision();

-- Financial terms and ownership never change in place. Only archival and its timestamp are lifecycle metadata; provider bindings live in their own table.
CREATE TRIGGER immutable_price_terms BEFORE UPDATE OR DELETE ON billing.prices
FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('archived', 'updated_at');
CREATE TRIGGER no_product_delete BEFORE DELETE ON billing.products
FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER no_product_truncate BEFORE TRUNCATE ON billing.products
FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER no_price_truncate BEFORE TRUNCATE ON billing.prices
FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();
COMMENT ON TABLE billing.products IS 'Catalog products; retire with archived, never delete. Retention: permanent, never pruned.';
COMMENT ON TABLE billing.prices IS 'Immutable financial price versions; retire with archived, never delete. Retention: permanent, never pruned.';

CREATE OR REPLACE FUNCTION billing.guard_product_identity() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.merchant_id IS DISTINCT FROM OLD.merchant_id OR NEW.key IS DISTINCT FROM OLD.key THEN
        RAISE EXCEPTION 'product identity is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

-- Products remain mutable. Their revision is an observation of current state,
-- not a separately retained product version or a caller-managed sequence.
ALTER TABLE billing.products ADD COLUMN revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0);
CREATE FUNCTION billing.assign_product_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        NEW.revision := COALESCE(NEW.revision, 0);
    ELSIF (to_jsonb(NEW)-ARRAY['updated_at','revision']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['updated_at','revision']) THEN
        NEW.revision := OLD.revision + 1;
    ELSE
        NEW.revision := OLD.revision;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER product_revision BEFORE INSERT OR UPDATE ON billing.products
FOR EACH ROW EXECUTE FUNCTION billing.assign_product_revision();
