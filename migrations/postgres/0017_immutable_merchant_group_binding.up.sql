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

-- A committed retirement is resumed using the captured group binding. It is
-- distinct from an operator's reversible directory-only soft deletion.
ALTER TABLE openrails.merchants
    ADD COLUMN retired_at timestamptz,
    ADD COLUMN group_release_completed_at timestamptz;
CREATE INDEX idx_merchants_pending_group_release
    ON openrails.merchants(retired_at, id)
    WHERE retired_at IS NOT NULL AND group_release_completed_at IS NULL;

-- The trigger needs the purge journal despite caller RLS context. It can inspect
-- only the merchant being updated and returns no journal data to the caller.
CREATE FUNCTION openrails.guard_merchant_restore() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO pg_catalog, openrails AS $$
BEGIN
    IF OLD.retired_at IS NOT NULL AND NEW.retired_at IS DISTINCT FROM OLD.retired_at THEN
        RAISE EXCEPTION 'merchant retirement is irreversible' USING ERRCODE='23514';
    END IF;
    IF NEW.deleted_at IS NULL AND NEW.status='active' THEN
        PERFORM openrails.assert_cross_merchant_reader();
        IF (
        OLD.retired_at IS NOT NULL OR EXISTS (
            SELECT 1 FROM openrails.destructive_runs r
             WHERE r.merchant_id=OLD.id AND r.kind='merchant_purge'
               AND r.affected->>'database_purged'='true'
        )
    ) THEN
        RAISE EXCEPTION 'retired or purged merchant cannot be restored' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION openrails.guard_merchant_restore() FROM PUBLIC;
CREATE TRIGGER guard_merchant_restore BEFORE UPDATE ON openrails.merchants
    FOR EACH ROW EXECUTE FUNCTION openrails.guard_merchant_restore();
