DROP INDEX IF EXISTS openrails.uq_merchants_webhook_host;

ALTER TABLE openrails.merchants
    DROP COLUMN IF EXISTS name,
    DROP COLUMN IF EXISTS plan,
    DROP COLUMN IF EXISTS region,
    DROP COLUMN IF EXISTS billing_tier,
    DROP COLUMN IF EXISTS stripe_account_id,
    DROP COLUMN IF EXISTS webhook_host,
    DROP COLUMN IF EXISTS webhook_path;
