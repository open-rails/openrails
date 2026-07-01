ALTER TABLE openrails.payment_provider_accounts
    ADD COLUMN archived boolean NOT NULL DEFAULT false;

UPDATE openrails.payment_provider_accounts
   SET archived = (routing = 'legacy' OR status = 'disabled'),
       replaced_at = CASE
           WHEN (routing = 'legacy' OR status = 'disabled') THEN COALESCE(replaced_at, now())
           ELSE replaced_at
       END,
       updated_at = now();

DROP INDEX IF EXISTS openrails.uq_payment_provider_accounts_enabled_primary;

ALTER TABLE openrails.payment_provider_accounts
    DROP CONSTRAINT IF EXISTS payment_provider_accounts_routing_check,
    DROP CONSTRAINT IF EXISTS payment_provider_accounts_status_check,
    DROP COLUMN routing,
    DROP COLUMN status;

CREATE INDEX idx_payment_provider_accounts_new_work
    ON openrails.payment_provider_accounts (merchant_id, rail, environment, created_at DESC, id DESC)
    WHERE archived = false;

COMMENT ON COLUMN openrails.payment_provider_accounts.archived IS
    'Drain-only provider-account lifecycle flag. false means eligible for new work; true remains addressable for existing obligations and inbound provider events.';
