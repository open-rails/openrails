-- Add Stripe/admin/nmi processors to enum (idempotent) and store processor customer IDs.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_enum e
        JOIN pg_type t ON t.oid = e.enumtypid
        JOIN pg_namespace n ON n.oid = t.typnamespace
        WHERE t.typname = 'processor_type' AND n.nspname = 'billing' AND e.enumlabel = 'stripe'
    ) THEN
        ALTER TYPE billing.processor_type ADD VALUE 'stripe';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_enum e
        JOIN pg_type t ON t.oid = e.enumtypid
        JOIN pg_namespace n ON n.oid = t.typnamespace
        WHERE t.typname = 'processor_type' AND n.nspname = 'billing' AND e.enumlabel = 'admin'
    ) THEN
        ALTER TYPE billing.processor_type ADD VALUE 'admin';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_enum e
        JOIN pg_type t ON t.oid = e.enumtypid
        JOIN pg_namespace n ON n.oid = t.typnamespace
        WHERE t.typname = 'processor_type' AND n.nspname = 'billing' AND e.enumlabel = 'nmi'
    ) THEN
        ALTER TYPE billing.processor_type ADD VALUE 'nmi';
    END IF;
END$$;

CREATE TABLE IF NOT EXISTS billing.processor_customers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    processor TEXT NOT NULL,
    customer_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    UNIQUE(user_id, processor),
    UNIQUE(processor, customer_id)
);

CREATE INDEX IF NOT EXISTS idx_processor_customers_user ON billing.processor_customers(user_id);
