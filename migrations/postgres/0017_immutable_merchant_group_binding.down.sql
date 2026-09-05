SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '60s';
DROP TRIGGER immutable_merchant_group_binding ON openrails.merchants;
DROP FUNCTION openrails.guard_merchant_group_binding();
DROP INDEX openrails.uq_merchants_permission_group_id;
CREATE UNIQUE INDEX uq_merchants_permission_group_id
    ON openrails.merchants(permission_group_id)
    WHERE permission_group_id IS NOT NULL AND deleted_at IS NULL;

DROP TRIGGER guard_merchant_restore ON openrails.merchants;
DROP FUNCTION openrails.guard_merchant_restore();
DROP INDEX openrails.idx_merchants_pending_group_release;
ALTER TABLE openrails.merchants DROP COLUMN group_release_completed_at, DROP COLUMN retired_at;
