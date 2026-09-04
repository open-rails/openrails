SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '60s';

-- A retired billing identity retains its AuthKit group binding. Reusing a name
-- cannot create a second billing identity for the same immutable group.
DROP INDEX openrails.uq_merchants_permission_group_id;
CREATE UNIQUE INDEX uq_merchants_permission_group_id
    ON openrails.merchants(permission_group_id) WHERE permission_group_id IS NOT NULL;

CREATE FUNCTION openrails.guard_merchant_group_binding() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO pg_catalog, openrails AS $$
BEGIN
    IF OLD.permission_group_id IS NOT NULL AND OLD.permission_group_id IS DISTINCT FROM NEW.permission_group_id THEN
        RAISE EXCEPTION 'merchant group binding is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER immutable_merchant_group_binding
    BEFORE UPDATE OF permission_group_id ON openrails.merchants
    FOR EACH ROW EXECUTE FUNCTION openrails.guard_merchant_group_binding();
