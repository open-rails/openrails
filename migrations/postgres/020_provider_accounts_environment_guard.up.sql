-- Repair DBs that applied an earlier provider_accounts shape before #518 added
-- provider environments. Current code keys provider accounts by
-- (merchant_id, provider_type, environment, account_id).

ALTER TABLE openrails.provider_accounts
  ADD COLUMN IF NOT EXISTS environment text DEFAULT 'live'::text NOT NULL;

UPDATE openrails.provider_accounts
SET environment = 'live'
WHERE btrim(coalesce(environment, '')) = '';

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'provider_accounts_environment_check'
      AND conrelid = 'openrails.provider_accounts'::regclass
  ) THEN
    ALTER TABLE openrails.provider_accounts
      ADD CONSTRAINT provider_accounts_environment_check
      CHECK (environment = ANY (ARRAY['live'::text, 'test'::text]));
  END IF;
END
$$;

DROP INDEX IF EXISTS openrails.uq_provider_accounts_identity;
CREATE UNIQUE INDEX uq_provider_accounts_identity
  ON openrails.provider_accounts USING btree (merchant_id, provider_type, environment, account_id);

DROP INDEX IF EXISTS openrails.uq_provider_accounts_enabled_primary;
CREATE UNIQUE INDEX uq_provider_accounts_enabled_primary
  ON openrails.provider_accounts USING btree (merchant_id, provider_type, environment)
  WHERE (role = 'primary'::text AND status = 'enabled'::text);

COMMENT ON COLUMN openrails.provider_accounts.environment IS 'Provider environment: live or test. Live and test accounts are distinct identities and may each have their own primary.';
