SET lock_timeout = '10s';
SET statement_timeout = '300s';

CREATE EXTENSION IF NOT EXISTS btree_gist;

ALTER TABLE billing.entitlements
    ADD COLUMN IF NOT EXISTS source_id UUID;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'billing'
          AND table_name = 'entitlements'
          AND column_name = 'subscription_id'
    ) THEN
        UPDATE billing.entitlements
        SET source_id = subscription_id,
            source_type = 'subscription'
        WHERE source_id IS NULL
          AND subscription_id IS NOT NULL;
    END IF;

    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'billing'
          AND table_name = 'entitlements'
          AND column_name = 'payment_id'
    ) THEN
        UPDATE billing.entitlements
        SET source_id = payment_id,
            source_type = 'one_off'
        WHERE source_id IS NULL
          AND payment_id IS NOT NULL;
    END IF;
END$$;

CREATE INDEX IF NOT EXISTS idx_entitlements_source
    ON billing.entitlements(source_type, source_id)
    WHERE source_id IS NOT NULL;

DROP INDEX IF EXISTS billing.idx_entitlements_no_overlap;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'entitlements_no_overlap'
          AND connamespace = 'billing'::regnamespace
    ) THEN
        ALTER TABLE billing.entitlements
        ADD CONSTRAINT entitlements_no_overlap
        EXCLUDE USING gist (user_id WITH =, entitlement WITH =, period WITH &&)
        WHERE (revoked_at IS NULL AND deleted_at IS NULL);
    END IF;
END$$;
