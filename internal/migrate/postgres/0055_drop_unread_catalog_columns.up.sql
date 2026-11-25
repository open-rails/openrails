ALTER TABLE billing.provider_catalog_objects
    DROP CONSTRAINT provider_catalog_objects_evidence_check,
    DROP COLUMN evidence;

ALTER TABLE billing.product_entitlements
    DROP CONSTRAINT product_entitlements_removed_check,
    DROP CONSTRAINT product_entitlements_removed_by_check,
    DROP COLUMN removed_by;

CREATE OR REPLACE FUNCTION billing.guard_product_entitlement_history() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'product entitlement history is immutable' USING ERRCODE='23514';
    END IF;
    IF OLD.removed_at IS NOT NULL OR NEW.removed_at IS NULL
       OR (to_jsonb(NEW)-ARRAY['removed_at']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['removed_at']) THEN
        RAISE EXCEPTION 'a product entitlement row only closes, once' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
