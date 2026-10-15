-- Repair: none-needed Explicit tenant adoption leaves legacy merchants unbound.
ALTER TABLE billing.merchants ADD COLUMN tenant_id uuid;
ALTER TABLE billing.merchants ADD CONSTRAINT merchants_tenant_group_check
    CHECK (tenant_id IS NULL OR (permission_group_id IS NOT NULL AND permission_group_id = id::text));
CREATE UNIQUE INDEX merchants_tenant_id_key ON billing.merchants (tenant_id)
    WHERE tenant_id IS NOT NULL;
COMMENT ON COLUMN billing.merchants.tenant_id IS 'Explicit immutable business tenant associated one-to-one with this billing identity. NULL is the legacy unbound mode; never inferred from an issuer, name, default account tenant or request. The matching permission group has this merchant ID.';

CREATE FUNCTION billing.guard_merchant_tenant_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.tenant_id IS NOT NULL AND (
        NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR
        NEW.permission_group_id IS DISTINCT FROM OLD.permission_group_id
    ) THEN
        RAISE EXCEPTION 'merchant tenant binding is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER merchants_tenant_binding_immutable
    BEFORE UPDATE OF tenant_id, permission_group_id ON billing.merchants
    FOR EACH ROW EXECUTE FUNCTION billing.guard_merchant_tenant_binding();
