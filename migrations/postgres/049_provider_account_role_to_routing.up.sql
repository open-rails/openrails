ALTER TABLE openrails.provider_accounts
    RENAME COLUMN role TO routing;

ALTER TABLE openrails.provider_accounts
    RENAME CONSTRAINT provider_accounts_role_check TO provider_accounts_routing_check;

COMMENT ON COLUMN openrails.provider_accounts.routing IS 'primary routes new work by default; secondary is enabled but explicit/manual; legacy is for old rows/rebills/refunds/webhooks only.';

DROP INDEX IF EXISTS openrails.uq_provider_accounts_enabled_primary;
CREATE UNIQUE INDEX uq_provider_accounts_enabled_primary
    ON openrails.provider_accounts (merchant_id, provider_type, environment)
    WHERE routing = 'primary'::text AND status = 'enabled'::text;
