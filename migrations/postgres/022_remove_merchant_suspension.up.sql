-- Merchant lifecycle has one non-active state: soft-deleted.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM information_schema.columns
         WHERE table_schema = 'openrails'
           AND table_name = 'merchants'
           AND column_name = 'suspended_at'
    ) THEN
        UPDATE openrails.merchants
           SET status = 'deleted',
               deleted_at = COALESCE(deleted_at, suspended_at, current_timestamp),
               updated_at = current_timestamp
         WHERE status = 'suspended';
    ELSE
        UPDATE openrails.merchants
           SET status = 'deleted',
               deleted_at = COALESCE(deleted_at, current_timestamp),
               updated_at = current_timestamp
         WHERE status = 'suspended';
    END IF;
END $$;

ALTER TABLE openrails.merchants
    DROP COLUMN IF EXISTS suspended_at,
    DROP COLUMN IF EXISTS provisioned_at;

ALTER TABLE openrails.merchants
    DROP CONSTRAINT IF EXISTS merchants_status_check;

ALTER TABLE openrails.merchants
    ADD CONSTRAINT merchants_status_check CHECK (status = ANY (ARRAY['active'::text, 'deleted'::text]));
