SET lock_timeout = '10s';
SET statement_timeout = '300s';

ALTER TABLE billing.subscriptions
    ALTER COLUMN user_id TYPE TEXT USING user_id::text;

ALTER TABLE billing.entitlements
    ALTER COLUMN user_id TYPE TEXT USING user_id::text;

ALTER TABLE billing.payments
    ALTER COLUMN user_id TYPE TEXT USING user_id::text;

ALTER TABLE billing.notification_queue
    ALTER COLUMN user_id TYPE TEXT USING user_id::text;

ALTER TABLE billing.user_credit_balances
    ALTER COLUMN user_id TYPE TEXT USING user_id::text;

ALTER TABLE billing.credit_transactions
    ALTER COLUMN user_id TYPE TEXT USING user_id::text;

ALTER TABLE billing.credit_blocks
    ALTER COLUMN user_id TYPE TEXT USING user_id::text;

ALTER TABLE billing.processor_customers
    ALTER COLUMN user_id TYPE TEXT USING user_id::text;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_enum e
        JOIN pg_type t ON t.oid = e.enumtypid
        JOIN pg_namespace n ON n.oid = t.typnamespace
        WHERE n.nspname = 'billing'
          AND t.typname = 'processor_type'
          AND e.enumlabel = 'stripe'
    ) THEN
        ALTER TYPE billing.processor_type ADD VALUE 'stripe';
    END IF;
END$$;
