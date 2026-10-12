-- parent: 73 sha256:7cc96af952a10e925a8cb19996bb4d507ba26e7a4504213a81de88ef52237d8c
-- Repair: public IDs are derived from existing immutable product and price keys
-- and revisions. Private UUID foreign keys and accepted financial terms remain
-- unchanged. The internal zero-based revision is exposed as version revision+1.
CREATE FUNCTION billing.escape_price_key(key text) RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT SET search_path TO pg_catalog AS $$
DECLARE
    bytes bytea := convert_to(key, 'UTF8');
    result text := '';
    value integer;
BEGIN
    FOR i IN 0..length(bytes)-1 LOOP
        value := get_byte(bytes, i);
        IF value BETWEEN 65 AND 90 OR value BETWEEN 97 AND 122 OR value BETWEEN 48 AND 57 OR value IN (45, 46, 95, 126) THEN
            result := result || chr(value);
        ELSE
            result := result || '%' || upper(lpad(to_hex(value), 2, '0'));
        END IF;
    END LOOP;
    RETURN result;
END;
$$;

ALTER TABLE billing.prices ADD COLUMN public_id text;
ALTER TABLE billing.prices DISABLE TRIGGER immutable_price_terms;
ALTER TABLE billing.prices DISABLE TRIGGER catalog_authored_price;
UPDATE billing.prices price SET public_id = billing.escape_price_key(product.key)
    || ':' || billing.escape_price_key(price.key) || ':v' || (price.revision + 1)::text
FROM billing.products product
WHERE product.merchant_id = price.merchant_id AND product.id = price.product_id;
ALTER TABLE billing.prices ENABLE TRIGGER catalog_authored_price;
ALTER TABLE billing.prices ENABLE TRIGGER immutable_price_terms;
ALTER TABLE billing.prices ALTER COLUMN public_id SET NOT NULL;
-- An encoded legacy key can exceed a btree text entry's size. Hash only for
-- lookup; exact text comparison remains mandatory. Existing immutable tuple
-- uniqueness already makes the canonical public ID unique per merchant.
CREATE INDEX prices_public_id_idx ON billing.prices (merchant_id, sha256(public_id::bytea));

-- Archived-price pages must not scan the merchant's live catalog to find a
-- small historical subset; this also preserves their keyset ordering.
CREATE INDEX prices_archived_created_at_id_idx ON billing.prices (merchant_id, archived, created_at DESC, id DESC);

CREATE FUNCTION billing.assign_price_public_id() RETURNS trigger
LANGUAGE plpgsql SET search_path TO pg_catalog, billing AS $$
DECLARE product_key text;
BEGIN
    SELECT key INTO STRICT product_key FROM billing.products
    WHERE merchant_id = NEW.merchant_id AND id = NEW.product_id;
    NEW.public_id := billing.escape_price_key(product_key) || ':'
        || billing.escape_price_key(NEW.key) || ':v' || (NEW.revision + 1)::text;
    RETURN NEW;
END;
$$;
-- Alphabetically after price_revision: allocation is already serialized by
-- catalog_authored_price's merchant lock. Snapshot restores supply a revision.
CREATE TRIGGER price_z_public_id BEFORE INSERT ON billing.prices
FOR EACH ROW EXECUTE FUNCTION billing.assign_price_public_id();
COMMENT ON COLUMN billing.prices.public_id IS 'Public immutable ID product-key:price-key:v1. Percent-encoded keys; financial versions start at one. UUID id remains a private foreign key.';
