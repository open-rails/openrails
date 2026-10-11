-- parent: 69 sha256:cc323dbbea514efdedfbaed5cee269b802fc6705d5abca970bd44575868b870f
-- Repair: none-needed New nullable columns hold no value, so no row breaks the checks.
-- A product or price nothing ever referenced can be deleted: deleted_at marks
-- it (always archived too) and reads leave it out. A referenced one is only
-- archived. Ids are derived from the product key and the price terms, so
-- declaring a deleted key or the same terms again revives that row.
ALTER TABLE billing.products ADD COLUMN deleted_at timestamp with time zone;
ALTER TABLE billing.products ADD CONSTRAINT products_deleted_archived_check CHECK (((deleted_at IS NULL) OR archived));
COMMENT ON COLUMN billing.products.deleted_at IS 'When the product was deleted: nothing referenced it. Reads leave it out; declaring its key again revives it.';

ALTER TABLE billing.prices ADD COLUMN deleted_at timestamp with time zone;
ALTER TABLE billing.prices ADD CONSTRAINT prices_deleted_archived_check CHECK (((deleted_at IS NULL) OR archived));
COMMENT ON COLUMN billing.prices.deleted_at IS 'When the price was deleted, with its product or its key: nothing referenced it. Reads leave it out; declaring the same terms again revives it.';

-- A price's terms stay immutable; it may be archived and deleted.
DROP TRIGGER immutable_price_terms ON billing.prices;
CREATE TRIGGER immutable_price_terms BEFORE UPDATE OR DELETE ON billing.prices
FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('archived', 'updated_at', 'deleted_at');
