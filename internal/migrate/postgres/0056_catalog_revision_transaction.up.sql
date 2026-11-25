CREATE OR REPLACE FUNCTION billing.catalog_authored_write() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE mid uuid;
BEGIN
    IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
    IF TG_OP='DELETE' THEN mid := OLD.merchant_id; ELSE mid := NEW.merchant_id; END IF;
    IF current_setting('openrails.catalog_batch_merchant_id',true) IS DISTINCT FROM mid::text
        AND NOT (mid::text = ANY(string_to_array(COALESCE(current_setting('openrails.catalog_revision_merchant_ids',true),''),','))) THEN
        -- A legacy raw writer may already hold a child-row lock. Do not wait
        -- behind a merchant-first transaction while holding that child: refuse
        -- with a retryable serialization conflict instead of deadlocking.
        BEGIN
            PERFORM id FROM billing.merchants WHERE id=mid FOR NO KEY UPDATE NOWAIT;
        EXCEPTION WHEN lock_not_available THEN
            RAISE EXCEPTION 'concurrent catalog authoring; retry the transaction' USING ERRCODE='40001';
        END;
        UPDATE billing.merchants SET catalog_revision=catalog_revision+1 WHERE id=mid;
        PERFORM set_config('openrails.catalog_revision_merchant_ids',concat_ws(',',NULLIF(current_setting('openrails.catalog_revision_merchant_ids',true),''),mid::text),true);
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END $$;

CREATE OR REPLACE FUNCTION billing.catalog_authored_entitlements() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE mid uuid;
BEGIN
    FOR mid IN SELECT DISTINCT merchant_id FROM changed LOOP
        IF current_setting('openrails.catalog_batch_merchant_id',true) IS DISTINCT FROM mid::text
        AND NOT (mid::text = ANY(string_to_array(COALESCE(current_setting('openrails.catalog_revision_merchant_ids',true),''),','))) THEN
            BEGIN
                PERFORM id FROM billing.merchants WHERE id=mid FOR NO KEY UPDATE NOWAIT;
            EXCEPTION WHEN lock_not_available THEN
                RAISE EXCEPTION 'concurrent catalog authoring; retry the transaction' USING ERRCODE='40001';
            END;
            UPDATE billing.merchants SET catalog_revision=catalog_revision+1 WHERE id=mid;
            PERFORM set_config('openrails.catalog_revision_merchant_ids',concat_ws(',',NULLIF(current_setting('openrails.catalog_revision_merchant_ids',true),''),mid::text),true);
        END IF;
    END LOOP;
    RETURN NULL;
END $$;
