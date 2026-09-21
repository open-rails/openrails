-- parent: 1 sha256:3810f381d651c07cb38b001a73819229cf504fa380b5f29fa8ff85efa05801e3
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '300s';

-- Creator catalog ownership is business data within an explicitly selected
-- merchant. It does not create database roles or change merchant authority.
CREATE TABLE openrails.catalogs (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES openrails.merchants(id) ON DELETE RESTRICT,
    owner_subject text COLLATE "C",
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT catalogs_merchant_id_id_key UNIQUE (merchant_id, id),
    CONSTRAINT catalogs_owner_subject_nonempty CHECK (owner_subject IS NULL OR owner_subject <> '')
);
CREATE UNIQUE INDEX catalogs_one_default ON openrails.catalogs (merchant_id) WHERE owner_subject IS NULL;
CREATE UNIQUE INDEX catalogs_one_owner ON openrails.catalogs (merchant_id, owner_subject) WHERE owner_subject IS NOT NULL;
COMMENT ON TABLE openrails.catalogs IS 'Immutable catalog identity within one merchant. NULL owner_subject is its default merchant catalog; non-NULL is an opaque verified host subject. Subject namespace must be preserved on authorized archive relocation.';

CREATE FUNCTION openrails.guard_catalog_identity() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'openrails', 'pg_temp' AS $$
BEGIN
    IF TG_OP='DELETE' OR NEW.id IS DISTINCT FROM OLD.id
       OR NEW.merchant_id IS DISTINCT FROM OLD.merchant_id
       OR NEW.owner_subject IS DISTINCT FROM OLD.owner_subject THEN
        RAISE EXCEPTION 'catalog identity and ownership are immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER immutable_catalog_identity BEFORE UPDATE OR DELETE ON openrails.catalogs
FOR EACH ROW EXECUTE FUNCTION openrails.guard_catalog_identity();

CREATE FUNCTION openrails.ensure_default_catalog(p_merchant uuid) RETURNS uuid
LANGUAGE sql SET search_path TO 'pg_catalog', 'openrails', 'pg_temp' AS $$
    INSERT INTO openrails.catalogs (merchant_id)
    VALUES (p_merchant)
    ON CONFLICT (merchant_id) WHERE owner_subject IS NULL
    DO UPDATE SET updated_at=openrails.catalogs.updated_at
    RETURNING id;
$$;
REVOKE ALL ON FUNCTION openrails.ensure_default_catalog(uuid) FROM PUBLIC;

-- Backfill only merchants that already own products. Product/price UUIDs and
-- all purchase relationships remain unchanged; there is no default merchant.
INSERT INTO openrails.catalogs (merchant_id)
SELECT DISTINCT merchant_id FROM openrails.products;
ALTER TABLE openrails.products ADD COLUMN catalog_id uuid;
UPDATE openrails.products p SET catalog_id=c.id
FROM openrails.catalogs c WHERE c.merchant_id=p.merchant_id AND c.owner_subject IS NULL;
ALTER TABLE openrails.products ADD CONSTRAINT products_catalog_present CHECK (catalog_id IS NOT NULL) NOT VALID;
-- Atomic upgrade already holds the product ALTER lock for the backfill.
-- Validation completes before any writer can observe a partial catalog binding.
-- squawk-ignore constraint-missing-not-valid
ALTER TABLE openrails.products VALIDATE CONSTRAINT products_catalog_present;
-- The validated CHECK proves non-nullness, so PostgreSQL skips another table scan.
-- This migration already holds the product ALTER lock for its atomic backfill.
-- squawk-ignore adding-not-nullable-field
ALTER TABLE openrails.products ALTER COLUMN catalog_id SET NOT NULL;
ALTER TABLE openrails.products ADD CONSTRAINT products_catalog_fk
    FOREIGN KEY (merchant_id, catalog_id) REFERENCES openrails.catalogs(merchant_id, id) ON DELETE RESTRICT NOT VALID;
-- Same atomic backfill boundary; this does not claim an online validation.
-- squawk-ignore constraint-missing-not-valid
ALTER TABLE openrails.products VALIDATE CONSTRAINT products_catalog_fk;
CREATE INDEX products_catalog_id ON openrails.products(merchant_id,catalog_id);

CREATE FUNCTION openrails.assign_product_catalog() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'openrails', 'pg_temp' AS $$
BEGIN
    IF NEW.catalog_id IS NULL THEN
        NEW.catalog_id := openrails.ensure_default_catalog(NEW.merchant_id);
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER assign_product_catalog BEFORE INSERT ON openrails.products
FOR EACH ROW EXECUTE FUNCTION openrails.assign_product_catalog();

CREATE FUNCTION openrails.guard_product_catalog_identity() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'openrails', 'pg_temp' AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.merchant_id IS DISTINCT FROM OLD.merchant_id
       OR NEW.catalog_id IS DISTINCT FROM OLD.catalog_id THEN
        RAISE EXCEPTION 'product catalog identity is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER immutable_product_catalog_identity BEFORE UPDATE ON openrails.products
FOR EACH ROW EXECUTE FUNCTION openrails.guard_product_catalog_identity();
