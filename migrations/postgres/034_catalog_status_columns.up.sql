-- Add catalog lifecycle status columns expected by current OpenRails code.
-- Older deployments used is_active booleans; keep the old columns in place for
-- audit/rollback visibility, but make status the canonical read path.

ALTER TABLE billing.products
    ADD COLUMN IF NOT EXISTS status TEXT DEFAULT 'active';

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'billing'
          AND table_name = 'products'
          AND column_name = 'is_active'
    ) THEN
        UPDATE billing.products
        SET status = CASE WHEN is_active THEN 'active' ELSE 'archived' END
        WHERE status IS NULL;
    ELSE
        UPDATE billing.products
        SET status = 'active'
        WHERE status IS NULL;
    END IF;
END $$;

ALTER TABLE billing.products
    ALTER COLUMN status SET DEFAULT 'active',
    ALTER COLUMN status SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'products_status_check'
          AND conrelid = 'billing.products'::regclass
    ) THEN
        ALTER TABLE billing.products
            ADD CONSTRAINT products_status_check
            CHECK (status IN ('draft', 'active', 'archived'));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_products_status ON billing.products(status);

ALTER TABLE billing.prices
    ADD COLUMN IF NOT EXISTS status TEXT DEFAULT 'active';

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'billing'
          AND table_name = 'prices'
          AND column_name = 'is_active'
    ) THEN
        UPDATE billing.prices
        SET status = CASE WHEN is_active THEN 'active' ELSE 'archived' END
        WHERE status IS NULL;
    ELSE
        UPDATE billing.prices
        SET status = 'active'
        WHERE status IS NULL;
    END IF;
END $$;

ALTER TABLE billing.prices
    ALTER COLUMN status SET DEFAULT 'active',
    ALTER COLUMN status SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'prices_status_check'
          AND conrelid = 'billing.prices'::regclass
    ) THEN
        ALTER TABLE billing.prices
            ADD CONSTRAINT prices_status_check
            CHECK (status IN ('draft', 'active', 'archived'));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_prices_status ON billing.prices(status);
